/*
Copyright 2026 Odoo K8s Operator.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	cxInstMissingSecret = "cx-inst-missing-secret"
	cxMissingSecret     = "cx-missing-pg"
	cxInstAddon         = "cx-inst-addon"
	cxAddon             = "cx-addon"
	cxAddonClash        = "cx-addon-clash"
	cxInstUpgrade       = "cx-inst-upgrade"
	cxUpgrade           = "cx-upgrade"
	cxDatabase          = "cx_db"
	cxSecret            = "cx-pg"
	cxModuleClash       = "module geox is already provided by addon other"
	cxEventSyncFailed   = "Warning SyncFailed"
	cxEventSyncStarted  = "Normal SyncStarted"
)

// cxRecorder returns a recorder whose events can be read with cxDrain.
func cxRecorder() *events.FakeRecorder {
	return events.NewFakeRecorder(50)
}

// cxDrain returns every event recorded so far, without blocking.
func cxDrain(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// cxBoundInstance creates an OdooInstance and its addons PVC as Bound, which is what an
// addon waits for before its sync Job starts.
func cxBoundInstance(ctx context.Context, name string) {
	createInstance(ctx, name, func(inst *odoov1.OdooInstance) {
		inst.Spec.Postgres = odoov1.PostgresSpec{Database: cxDatabase, PasswordSecret: cxSecret}
	})
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: addonsPVCName(name), Namespace: testNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	Expect(k8sClient.Create(ctx, pvc)).To(Succeed())
	pvc.Status.Phase = corev1.ClaimBound
	Expect(k8sClient.Status().Update(ctx, pvc)).To(Succeed())
	DeferCleanup(func() { oaDeletePVC(context.Background(), addonsPVCName(name)) })
}

// cxAddonFor creates an OdooAddon for instance and removes it, with its Jobs, when the spec ends.
func cxAddonFor(ctx context.Context, name, instance string) {
	addon := &odoov1.OdooAddon{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: odoov1.OdooAddonSpec{
			GitUrl:      testHTTPSURL,
			GitRef:      testBranchRef,
			InstanceRef: odoov1.OdooInstanceRef{Name: instance},
		},
	}
	Expect(k8sClient.Create(ctx, addon)).To(Succeed())
	DeferCleanup(func() {
		bg := context.Background()
		background := client.PropagationPolicy(metav1.DeletePropagationBackground)
		_ = k8sClient.Delete(bg, &odoov1.OdooAddon{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}})
		oaFinalize(bg, name)
		_ = k8sClient.Delete(bg, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: syncJobName(name), Namespace: testNamespace}}, background)
		_ = k8sClient.Delete(bg, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: cleanupJobName(name), Namespace: testNamespace}}, background)
		_ = k8sClient.DeleteAllOf(bg, &corev1.Pod{}, client.InNamespace(testNamespace),
			client.MatchingLabels{oaEnvJobNameLabel: syncJobName(name)})
	})
}

func cxReconcileAddon(r *OdooAddonReconciler, name string) ctrl.Result {
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace},
	})
	Expect(err).NotTo(HaveOccurred())
	return res
}

func cxGetAddon(ctx context.Context, name string) *odoov1.OdooAddon {
	addon := &odoov1.OdooAddon{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, addon)).To(Succeed())
	return addon
}

// cxFailSync runs the first sync Job of addon to a failed state. The Job's pod carries the logs
// the reconciler reads, so the caller's clientset decides the failure text.
func cxFailSync(ctx context.Context, r *OdooAddonReconciler, name string) {
	cxReconcileAddon(r, name)
	job, err := getNamedJob(syncJobName(name))
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient.Create(ctx, oaEnvLogsFor(job))).To(Succeed())
	setNamedJobStatus(syncJobName(name), false)
}

var _ = Describe("OdooInstance conditions and events", func() {
	ctx := context.Background()

	It("reports a missing Secret as Degraded with its reason and message, and emits ReconcileFailed", func() {
		createInstance(ctx, cxInstMissingSecret, func(inst *odoov1.OdooInstance) {
			inst.Spec.Postgres.PasswordSecret = cxMissingSecret
		})
		rec := cxRecorder()
		r := &OdooInstanceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Recorder: rec}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: cxInstMissingSecret, Namespace: testNamespace}}

		_, err := r.Reconcile(ctx, req)
		Expect(err).To(HaveOccurred())

		inst := fetchInstance(ctx, cxInstMissingSecret)
		Expect(inst.Status.Phase).To(Equal(phaseFailing))
		Expect(inst.Status.Ready).To(BeFalse())
		ready := meta.FindStatusCondition(inst.Status.Conditions, odoov1.ConditionTypeReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		degraded := meta.FindStatusCondition(inst.Status.Conditions, odoov1.ConditionTypeDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
		Expect(degraded.Reason).To(Equal(odoov1.ReasonMissingSecret))
		Expect(degraded.Message).To(ContainSubstring(cxMissingSecret))
		Expect(degraded.ObservedGeneration).To(Equal(inst.Generation))
		Expect(inst.Status.Message).To(Equal(degraded.Message))
		Expect(cxDrain(rec)).To(ContainElement(ContainSubstring("Warning ReconcileFailed")))

		By("clearing Degraded once the Secret exists")
		createSecret(ctx, cxMissingSecret, map[string][]byte{secretKeyPassword: []byte("pw")})
		_, err = r.Reconcile(ctx, req)
		Expect(err).NotTo(HaveOccurred())

		inst = fetchInstance(ctx, cxInstMissingSecret)
		degraded = meta.FindStatusCondition(inst.Status.Conditions, odoov1.ConditionTypeDegraded)
		Expect(degraded.Status).To(Equal(metav1.ConditionFalse))
		Expect(degraded.Reason).To(Equal(odoov1.ReasonAsExpected))
		Expect(inst.Status.Message).To(ContainSubstring("replicas"))
		recovery := cxDrain(rec)
		Expect(recovery).NotTo(ContainElement(ContainSubstring("Warning")), "recovery must not emit a warning")
		Expect(recovery).To(ContainElement(ContainSubstring("Normal RolledOut")))
	})
})

var _ = Describe("OdooAddon conditions and events", func() {
	ctx := context.Background()

	It("marks a failed sync Degraded with the reason, emits its events, and stops writing during the cooldown", func() {
		cxBoundInstance(ctx, cxInstAddon)
		cxAddonFor(ctx, cxAddon, cxInstAddon)
		rec := cxRecorder()
		r := &OdooAddonReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clientset: fake.NewClientset(), Recorder: rec}

		cxFailSync(ctx, r, cxAddon)
		Expect(cxReconcileAddon(r, cxAddon).RequeueAfter).To(Equal(failureBackoff))

		addon := cxGetAddon(ctx, cxAddon)
		Expect(addon.Status.Phase).To(Equal(phaseFailed))
		degraded := meta.FindStatusCondition(addon.Status.Conditions, odoov1.ConditionTypeDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
		Expect(degraded.Reason).To(Equal(odoov1.ReasonSyncFailed))
		Expect(degraded.Message).To(ContainSubstring(oaEnvBackoffRsn))
		Expect(degraded.Message).To(ContainSubstring(oaEnvFakeLogs))
		Expect(addon.Status.Message).To(Equal(degraded.Message))
		events := cxDrain(rec)
		Expect(events).To(ContainElement(ContainSubstring(cxEventSyncStarted)))
		Expect(events).To(ContainElement(ContainSubstring(cxEventSyncFailed)))

		By("not writing the status again while the failure cooldown runs")
		before := addon.ResourceVersion
		Expect(cxReconcileAddon(r, cxAddon).RequeueAfter).To(BeNumerically(">", 0))
		Expect(cxGetAddon(ctx, cxAddon).ResourceVersion).To(Equal(before))
		Expect(cxDrain(rec)).To(BeEmpty(), "a cooldown pass must not emit events")
	})

	It("reports a module clash as ModuleConflict and emits a ModuleConflict event", func() {
		cxBoundInstance(ctx, cxInstAddon+"-clash")
		cxAddonFor(ctx, cxAddonClash, cxInstAddon+"-clash")
		rec := cxRecorder()
		clientset := oaLogsClientset{Interface: fake.NewClientset(), logs: cxModuleClash + "\n"}
		r := &OdooAddonReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clientset: clientset, Recorder: rec}

		cxFailSync(ctx, r, cxAddonClash)
		cxReconcileAddon(r, cxAddonClash)

		addon := cxGetAddon(ctx, cxAddonClash)
		degraded := meta.FindStatusCondition(addon.Status.Conditions, odoov1.ConditionTypeDegraded)
		Expect(degraded).NotTo(BeNil())
		Expect(degraded.Reason).To(Equal(odoov1.ReasonModuleConflict))
		Expect(degraded.Message).To(ContainSubstring("geox"))
		Expect(cxDrain(rec)).To(ContainElement(ContainSubstring("Warning ModuleConflict")))
	})
})

var _ = Describe("OdooUpgrade conditions and events", func() {
	ctx := context.Background()

	It("sets Succeeded=True and emits Started and Succeeded when the Job completes", func() {
		createInstance(ctx, cxInstUpgrade, func(inst *odoov1.OdooInstance) {
			inst.Spec.Postgres = odoov1.PostgresSpec{Database: cxDatabase, PasswordSecret: cxSecret}
		})
		up := &odoov1.OdooUpgrade{
			ObjectMeta: metav1.ObjectMeta{Name: cxUpgrade, Namespace: testNamespace},
			Spec: odoov1.OdooUpgradeSpec{
				InstanceRef: odoov1.OdooInstanceRef{Name: cxInstUpgrade},
				Modules:     []string{tiEnvModule},
			},
		}
		Expect(k8sClient.Create(ctx, up)).To(Succeed())
		DeferCleanup(func() {
			bg := context.Background()
			_ = k8sClient.Delete(bg, &odoov1.OdooUpgrade{ObjectMeta: metav1.ObjectMeta{Name: cxUpgrade, Namespace: testNamespace}})
			_ = k8sClient.Delete(bg, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: upgradeJobName(cxUpgrade), Namespace: testNamespace}},
				client.PropagationPolicy(metav1.DeletePropagationBackground))
		})

		rec := cxRecorder()
		r := &OdooUpgradeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Clientset: fake.NewClientset(), Recorder: rec}
		reconcileUpgrade := func() {
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cxUpgrade, Namespace: testNamespace}})
			Expect(err).NotTo(HaveOccurred())
		}

		reconcileUpgrade()
		running := &odoov1.OdooUpgrade{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cxUpgrade, Namespace: testNamespace}, running)).To(Succeed())
		Expect(running.Status.Phase).To(Equal(odoov1.UpgradePhaseRunning))
		progressing := meta.FindStatusCondition(running.Status.Conditions, odoov1.ConditionTypeProgressing)
		Expect(progressing).NotTo(BeNil())
		Expect(progressing.Status).To(Equal(metav1.ConditionTrue))
		Expect(progressing.Reason).To(Equal(odoov1.ReasonRunning))
		Expect(cxDrain(rec)).To(ContainElement(ContainSubstring("Normal Started")))

		setNamedJobStatus(upgradeJobName(cxUpgrade), true)
		reconcileUpgrade()

		done := &odoov1.OdooUpgrade{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cxUpgrade, Namespace: testNamespace}, done)).To(Succeed())
		Expect(done.Status.Phase).To(Equal(odoov1.UpgradePhaseSucceeded))
		succeeded := meta.FindStatusCondition(done.Status.Conditions, odoov1.ConditionTypeSucceeded)
		Expect(succeeded).NotTo(BeNil())
		Expect(succeeded.Status).To(Equal(metav1.ConditionTrue))
		Expect(succeeded.Reason).To(Equal(odoov1.ReasonComplete))
		Expect(cxDrain(rec)).To(ContainElement(ContainSubstring("Normal Succeeded")))
	})
})
