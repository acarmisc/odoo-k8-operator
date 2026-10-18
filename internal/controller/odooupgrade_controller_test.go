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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	tuEnvInstA      = "upg-env-inst-a"
	tuEnvInstB      = "upg-env-inst-b"
	tuEnvInstC      = "upg-env-inst-c"
	tuEnvUpgFirst   = "upg-env-first"
	tuEnvUpgSecond  = "upg-env-second"
	tuEnvUpgThird   = "upg-env-third"
	tuEnvDatabase   = "odoo_env"
	tuEnvSecret     = "upg-env-pg"
	tuEnvLogLine    = "fake logs"
	tuEnvBackoffMsg = "Job has reached the specified backoff limit"
	tuEnvBackoffRsn = "BackoffLimitExceeded"
)

var _ = Describe("OdooUpgrade Controller", func() {
	ctx := context.Background()

	newReconciler := func(clientset kubernetes.Interface) *OdooUpgradeReconciler {
		return &OdooUpgradeReconciler{
			Client:    k8sClient,
			Scheme:    k8sClient.Scheme(),
			Clientset: clientset,
		}
	}

	reconcile := func(r *OdooUpgradeReconciler, name string) ctrl.Result {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace}})
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	getUpgrade := func(name string) *odoov1.OdooUpgrade {
		up := &odoov1.OdooUpgrade{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, up)).To(Succeed())
		return up
	}

	getJob := func(name string) (*batchv1.Job, error) {
		job := &batchv1.Job{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, job)
		return job, err
	}

	createInstance := func(name string) {
		inst := &odoov1.OdooInstance{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: odoov1.OdooInstanceSpec{
				Postgres: odoov1.PostgresSpec{Database: tuEnvDatabase, PasswordSecret: tuEnvSecret},
			},
		}
		Expect(k8sClient.Create(ctx, inst)).To(Succeed())
	}

	createUpgrade := func(name, instance string, modules []string) {
		up := &odoov1.OdooUpgrade{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: odoov1.OdooUpgradeSpec{
				InstanceRef: odoov1.OdooInstanceRef{Name: instance},
				Modules:     modules,
			},
		}
		Expect(k8sClient.Create(ctx, up)).To(Succeed())
	}

	// setJobStatus writes Job status the way the Job controller would for a finished Job.
	setJobStatus := func(name string, succeeded bool) {
		job, err := getJob(name)
		Expect(err).NotTo(HaveOccurred())
		now := metav1.Now()
		job.Status.StartTime = &now
		if succeeded {
			job.Status.Succeeded = 1
			job.Status.CompletionTime = &now
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: "CompletionsReached"},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
			}
		} else {
			job.Status.Failed = 1
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: tuEnvBackoffRsn, Message: tuEnvBackoffMsg},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: now, Reason: tuEnvBackoffRsn, Message: tuEnvBackoffMsg},
			}
		}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
	}

	// cleanup removes everything a spec created. envtest has no garbage collector, so a
	// Job deleted with the default orphan propagation would linger on its finalizer and
	// make the next spec adopt it; background propagation deletes it outright.
	cleanup := func(upgrades []string, instances []string) {
		background := client.PropagationPolicy(metav1.DeletePropagationBackground)
		for _, name := range upgrades {
			_ = k8sClient.Delete(ctx, &odoov1.OdooUpgrade{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}})
			_ = k8sClient.Delete(ctx, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: upgradeJobName(name), Namespace: testNamespace}}, background)
			_ = k8sClient.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: upgradeJobName(name) + "-pod", Namespace: testNamespace}})
		}
		for _, name := range instances {
			_ = k8sClient.Delete(ctx, &odoov1.OdooInstance{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}})
		}
	}

	Context("when an upgrade is created for an instance", func() {
		AfterEach(func() {
			cleanup([]string{tuEnvUpgFirst}, []string{tuEnvInstA})
		})

		It("creates a hardened Job with the expected arguments and marks the upgrade Running", func() {
			createInstance(tuEnvInstA)
			createUpgrade(tuEnvUpgFirst, tuEnvInstA, []string{tuModuleSale, tuModuleStock})

			res := reconcile(newReconciler(nil), tuEnvUpgFirst)
			Expect(res.RequeueAfter).To(BeNumerically(">", 0))

			up := getUpgrade(tuEnvUpgFirst)
			Expect(up.Status.Phase).To(Equal(odoov1.UpgradePhaseRunning))
			Expect(up.Status.JobName).To(Equal(tuEnvUpgFirst + "-run"))
			Expect(up.Status.StartTime).NotTo(BeNil())
			Expect(up.Status.ObservedGeneration).To(Equal(up.Generation))

			job, err := getJob(up.Status.JobName)
			Expect(err).NotTo(HaveOccurred())
			Expect(job.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := job.Spec.Template.Spec.Containers[0]
			Expect(container.Args).To(Equal([]string{
				upgradeFlagConfig, odooConfPath, upgradeFlagDB, tuEnvDatabase, upgradeFlagUpdate, tuModulesJoined, upgradeFlagStop, upgradeFlagNoHTTP,
			}))
			Expect(job.Spec.Template.Spec.RestartPolicy).To(Equal(corev1.RestartPolicyNever))
			Expect(*job.Spec.BackoffLimit).To(BeZero())
			Expect(*job.Spec.ActiveDeadlineSeconds).To(Equal(upgradeDefaultTimeout))
			Expect(job.Spec.Template.Labels).NotTo(HaveKey(labelInstance))
			Expect(job.OwnerReferences).To(HaveLen(1))
			Expect(job.OwnerReferences[0].Kind).To(Equal("OdooUpgrade"))
			Expect(*job.OwnerReferences[0].Controller).To(BeTrue())
		})

		It("adopts an existing Job when the status was lost", func() {
			createInstance(tuEnvInstA)
			createUpgrade(tuEnvUpgFirst, tuEnvInstA, []string{tuModuleSale})
			r := newReconciler(nil)
			reconcile(r, tuEnvUpgFirst)

			// Forget the Running status; the Job still exists, so Create hits AlreadyExists.
			up := getUpgrade(tuEnvUpgFirst)
			up.Status = odoov1.OdooUpgradeStatus{}
			Expect(k8sClient.Status().Update(ctx, up)).To(Succeed())

			reconcile(r, tuEnvUpgFirst)
			up = getUpgrade(tuEnvUpgFirst)
			Expect(up.Status.Phase).To(Equal(odoov1.UpgradePhaseRunning))
			Expect(up.Status.JobName).To(Equal(tuEnvUpgFirst + "-run"))
		})

		It("marks the upgrade Succeeded once the Job completes", func() {
			createInstance(tuEnvInstA)
			createUpgrade(tuEnvUpgFirst, tuEnvInstA, []string{tuModuleSale})
			r := newReconciler(nil)
			reconcile(r, tuEnvUpgFirst)

			setJobStatus(upgradeJobName(tuEnvUpgFirst), true)
			reconcile(r, tuEnvUpgFirst)

			up := getUpgrade(tuEnvUpgFirst)
			Expect(up.Status.Phase).To(Equal(odoov1.UpgradePhaseSucceeded))
			Expect(up.Status.CompletionTime).NotTo(BeNil())
		})

		It("is terminal: reconciling a Succeeded upgrade does nothing", func() {
			createInstance(tuEnvInstA)
			createUpgrade(tuEnvUpgFirst, tuEnvInstA, []string{tuModuleSale})
			r := newReconciler(nil)
			reconcile(r, tuEnvUpgFirst)
			setJobStatus(upgradeJobName(tuEnvUpgFirst), true)
			reconcile(r, tuEnvUpgFirst)

			reconcile(r, tuEnvUpgFirst)
			Expect(getUpgrade(tuEnvUpgFirst).Status.Phase).To(Equal(odoov1.UpgradePhaseSucceeded))
		})
	})

	Context("when the Job fails", func() {
		AfterEach(func() {
			cleanup([]string{tuEnvUpgSecond}, []string{tuEnvInstB})
		})

		It("marks the upgrade Failed with the condition message and the pod log tail", func() {
			createInstance(tuEnvInstB)
			createUpgrade(tuEnvUpgSecond, tuEnvInstB, []string{"all"})
			clientset := fake.NewClientset()
			r := newReconciler(clientset)
			reconcile(r, tuEnvUpgSecond)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      upgradeJobName(tuEnvUpgSecond) + "-pod",
					Namespace: testNamespace,
					Labels:    upgradeLabels(tuEnvUpgSecond),
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: upgradeContainerName, Image: "busybox"}},
				},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())

			setJobStatus(upgradeJobName(tuEnvUpgSecond), false)
			reconcile(r, tuEnvUpgSecond)

			up := getUpgrade(tuEnvUpgSecond)
			Expect(up.Status.Phase).To(Equal(odoov1.UpgradePhaseFailed))
			Expect(up.Status.CompletionTime).NotTo(BeNil())
			Expect(up.Status.Message).To(ContainSubstring(tuEnvBackoffRsn))
			Expect(up.Status.Message).To(ContainSubstring(tuEnvLogLine))
		})
	})

	Context("when several upgrades target the same instance", func() {
		AfterEach(func() {
			cleanup([]string{tuEnvUpgFirst, tuEnvUpgSecond, tuEnvUpgThird}, []string{tuEnvInstC})
		})

		It("keeps the second upgrade Pending while the first is Running", func() {
			createInstance(tuEnvInstC)
			createUpgrade(tuEnvUpgFirst, tuEnvInstC, []string{tuModuleSale})
			r := newReconciler(nil)
			reconcile(r, tuEnvUpgFirst)

			createUpgrade(tuEnvUpgSecond, tuEnvInstC, []string{"stock"})
			res := reconcile(r, tuEnvUpgSecond)
			Expect(res.RequeueAfter).To(Equal(upgradeRequeuePending))

			second := getUpgrade(tuEnvUpgSecond)
			Expect(second.Status.Phase).To(Equal(odoov1.UpgradePhasePending))
			Expect(second.Status.Message).To(ContainSubstring(tuEnvUpgFirst))
			Expect(second.Status.JobName).To(BeEmpty())
			_, err := getJob(upgradeJobName(tuEnvUpgSecond))
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no Job may exist for a queued upgrade")

			setJobStatus(upgradeJobName(tuEnvUpgFirst), true)
			reconcile(r, tuEnvUpgFirst)
			reconcile(r, tuEnvUpgSecond)
			Expect(getUpgrade(tuEnvUpgSecond).Status.Phase).To(Equal(odoov1.UpgradePhaseRunning))
		})

		It("waits for the instance before starting", func() {
			createUpgrade(tuEnvUpgThird, "upg-env-missing", []string{tuModuleSale})
			res := reconcile(newReconciler(nil), tuEnvUpgThird)
			Expect(res.RequeueAfter).To(Equal(upgradeRequeuePending))

			up := getUpgrade(tuEnvUpgThird)
			Expect(up.Status.Phase).To(Equal(odoov1.UpgradePhasePending))
			Expect(up.Status.Message).To(Equal(upgradeMsgWaitingInstance))
		})

		It("fails an invalid spec without creating a Job", func() {
			createInstance(tuEnvInstC)
			createUpgrade(tuEnvUpgThird, tuEnvInstC, []string{upgradeAllModules, tuModuleSale})
			reconcile(newReconciler(nil), tuEnvUpgThird)

			up := getUpgrade(tuEnvUpgThird)
			Expect(up.Status.Phase).To(Equal(odoov1.UpgradePhaseFailed))
			Expect(up.Status.JobName).To(BeEmpty())
			_, err := getJob(upgradeJobName(tuEnvUpgThird))
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})
	})
})
