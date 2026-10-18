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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	tgGateFlow        = "gate-flow"
	tgGateFailed      = "gate-failed"
	tgGateNoSpec      = "gate-no-spec"
	tgGateDatabase    = "odoo"
	tgGateModule      = "sale"
	tgGateGitURL      = "https://example.com/acme/odoo-addons.git"
	tgGateCommitA     = "aaa1111"
	tgGateCommitB     = "bbb2222"
	tgGatePhaseSynced = "Synced"
)

// tgGateAddon names the OdooAddon bound to an instance; each instance gets its own.
func tgGateAddon(instanceName string) string {
	return instanceName + "-addon"
}

// tgGateRevision is the addons hash for a single ready addon at the given commit.
func tgGateRevision(instanceName, commit string) string {
	return sha256Hex(tgGateAddon(instanceName) + "=" + commit)
}

func tgGateUpgradeName(instanceName, commit string) string {
	return gateUpgradeName(instanceName, tgGateRevision(instanceName, commit))
}

func tgGateCreateAddon(ctx context.Context, instanceName string) {
	addon := &odoov1.OdooAddon{
		ObjectMeta: metav1.ObjectMeta{Name: tgGateAddon(instanceName), Namespace: testNamespace},
		Spec: odoov1.OdooAddonSpec{
			GitUrl:      tgGateGitURL,
			InstanceRef: odoov1.OdooInstanceRef{Name: instanceName},
		},
	}
	Expect(k8sClient.Create(ctx, addon)).To(Succeed())
	DeferCleanup(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), addon))).To(Succeed())
	})
}

// tgGateSetCommit marks the addon Ready and Synced at the given commit.
func tgGateSetCommit(ctx context.Context, instanceName, commit string) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		addon := &odoov1.OdooAddon{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: tgGateAddon(instanceName), Namespace: testNamespace}, addon); err != nil {
			return err
		}
		addon.Status.Ready = true
		addon.Status.Phase = tgGatePhaseSynced
		addon.Status.ClonedCommit = commit
		return k8sClient.Status().Update(ctx, addon)
	})).To(Succeed())
}

// tgGateSetUpgradePhase plays the part of the OdooUpgrade controller in tests.
func tgGateSetUpgradePhase(ctx context.Context, upgradeName, phase string) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		upgrade := &odoov1.OdooUpgrade{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: upgradeName, Namespace: testNamespace}, upgrade); err != nil {
			return err
		}
		upgrade.Status.Phase = phase
		return k8sClient.Status().Update(ctx, upgrade)
	})).To(Succeed())
}

func tgGateFetchUpgrade(ctx context.Context, upgradeName string) *odoov1.OdooUpgrade {
	upgrade := &odoov1.OdooUpgrade{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: upgradeName, Namespace: testNamespace}, upgrade)).To(Succeed())
	return upgrade
}

func tgGateRegisterUpgradeCleanup(upgradeName string) {
	DeferCleanup(func() {
		upgrade := &odoov1.OdooUpgrade{ObjectMeta: metav1.ObjectMeta{Name: upgradeName, Namespace: testNamespace}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), upgrade))).To(Succeed())
	})
}

// tgGateReconcile runs one pass and returns its result; the result matters for the failed-upgrade requeue.
func tgGateReconcile(ctx context.Context, name string) reconcile.Result {
	reconciler := &OdooInstanceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	result, err := reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace},
	})
	Expect(err).NotTo(HaveOccurred())
	return result
}

func tgGatePodAddonsHash(ctx context.Context, instanceName string) string {
	return fetchDeployment(ctx, instanceName).Spec.Template.Annotations[annotationAddonsHash]
}

var _ = Describe("OdooInstance upgrade gate", func() {
	It("holds the pods on the applied revision until the OdooUpgrade succeeds", func() {
		ctx := context.Background()
		createInstance(ctx, tgGateFlow, func(i *odoov1.OdooInstance) {
			i.Spec.Postgres.Database = tgGateDatabase
			i.Spec.Upgrade = &odoov1.UpgradeSpec{Modules: []string{tgGateModule}, TimeoutSeconds: int64Ptr(600)}
		})
		tgGateCreateAddon(ctx, tgGateFlow)
		tgGateSetCommit(ctx, tgGateFlow, tgGateCommitA)

		// First reconcile with the feature: record the hash and roll out, no upgrade.
		tgGateReconcile(ctx, tgGateFlow)
		Expect(tgGatePodAddonsHash(ctx, tgGateFlow)).To(Equal(tgGateRevision(tgGateFlow, tgGateCommitA)))
		Expect(fetchInstance(ctx, tgGateFlow).Status.AppliedAddonsHash).To(Equal(tgGateRevision(tgGateFlow, tgGateCommitA)))
		Expect(fetchInstance(ctx, tgGateFlow).Status.ActiveUpgrade).To(BeEmpty())

		// Addon moves to a new commit: an upgrade is created, the pods keep the old hash.
		tgGateSetCommit(ctx, tgGateFlow, tgGateCommitB)
		upgradeName := tgGateUpgradeName(tgGateFlow, tgGateCommitB)
		tgGateRegisterUpgradeCleanup(upgradeName)
		tgGateReconcile(ctx, tgGateFlow)

		upgrade := tgGateFetchUpgrade(ctx, upgradeName)
		Expect(upgrade.Spec.InstanceRef.Name).To(Equal(tgGateFlow))
		Expect(upgrade.Spec.Modules).To(Equal([]string{tgGateModule}))
		Expect(upgrade.Spec.TimeoutSeconds).NotTo(BeNil())
		Expect(*upgrade.Spec.TimeoutSeconds).To(Equal(int64(600)))
		Expect(upgrade.Labels).To(HaveKeyWithValue(gateLabelInstance, tgGateFlow))
		Expect(metav1.IsControlledBy(upgrade, fetchInstance(ctx, tgGateFlow))).To(BeTrue())

		Expect(tgGatePodAddonsHash(ctx, tgGateFlow)).To(Equal(tgGateRevision(tgGateFlow, tgGateCommitA)),
			"pods must not roll before the upgrade succeeded")
		instance := fetchInstance(ctx, tgGateFlow)
		Expect(instance.Status.ActiveUpgrade).To(Equal(upgradeName))
		Expect(instance.Status.AppliedAddonsHash).To(Equal(tgGateRevision(tgGateFlow, tgGateCommitA)))
		Expect(instance.Status.Phase).To(Equal(gatePhaseUpgrading))

		// A repeat reconcile reuses the existing upgrade and keeps holding.
		tgGateReconcile(ctx, tgGateFlow)
		Expect(tgGateFetchUpgrade(ctx, upgradeName).UID).To(Equal(upgrade.UID))
		Expect(tgGatePodAddonsHash(ctx, tgGateFlow)).To(Equal(tgGateRevision(tgGateFlow, tgGateCommitA)))

		// Upgrade succeeds: the new hash is applied and rolled out in the same pass.
		tgGateSetUpgradePhase(ctx, upgradeName, odoov1.UpgradePhaseSucceeded)
		tgGateReconcile(ctx, tgGateFlow)
		Expect(tgGatePodAddonsHash(ctx, tgGateFlow)).To(Equal(tgGateRevision(tgGateFlow, tgGateCommitB)))
		instance = fetchInstance(ctx, tgGateFlow)
		Expect(instance.Status.AppliedAddonsHash).To(Equal(tgGateRevision(tgGateFlow, tgGateCommitB)))
		Expect(instance.Status.ActiveUpgrade).To(BeEmpty())
	})

	It("keeps the old hash and reports UpgradeFailed, then recreates the upgrade once it is deleted", func() {
		ctx := context.Background()
		createInstance(ctx, tgGateFailed, func(i *odoov1.OdooInstance) {
			i.Spec.Postgres.Database = tgGateDatabase
			i.Spec.Upgrade = &odoov1.UpgradeSpec{Modules: []string{tgGateModule}}
		})
		tgGateCreateAddon(ctx, tgGateFailed)
		tgGateSetCommit(ctx, tgGateFailed, tgGateCommitA)
		tgGateReconcile(ctx, tgGateFailed)

		tgGateSetCommit(ctx, tgGateFailed, tgGateCommitB)
		upgradeName := tgGateUpgradeName(tgGateFailed, tgGateCommitB)
		tgGateRegisterUpgradeCleanup(upgradeName)
		tgGateReconcile(ctx, tgGateFailed)

		tgGateSetUpgradePhase(ctx, upgradeName, odoov1.UpgradePhaseFailed)
		result := tgGateReconcile(ctx, tgGateFailed)
		Expect(result.RequeueAfter).To(Equal(gateFailedRequeue))

		instance := fetchInstance(ctx, tgGateFailed)
		Expect(instance.Status.Phase).To(Equal(gatePhaseUpgradeFailed))
		Expect(instance.Status.Ready).To(BeFalse())
		Expect(instance.Status.ActiveUpgrade).To(Equal(upgradeName))
		Expect(instance.Status.AppliedAddonsHash).To(Equal(tgGateRevision(tgGateFailed, tgGateCommitA)))
		Expect(tgGatePodAddonsHash(ctx, tgGateFailed)).To(Equal(tgGateRevision(tgGateFailed, tgGateCommitA)))

		// Operator retry: delete the failed upgrade; the next pass creates a fresh one.
		Expect(k8sClient.Delete(ctx, tgGateFetchUpgrade(ctx, upgradeName))).To(Succeed())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Name: upgradeName, Namespace: testNamespace}, &odoov1.OdooUpgrade{}))).To(BeTrue())
		tgGateReconcile(ctx, tgGateFailed)

		recreated := tgGateFetchUpgrade(ctx, upgradeName)
		Expect(recreated.Status.Phase).NotTo(Equal(odoov1.UpgradePhaseFailed))
		instance = fetchInstance(ctx, tgGateFailed)
		Expect(instance.Status.Phase).To(Equal(gatePhaseUpgrading))
		Expect(tgGatePodAddonsHash(ctx, tgGateFailed)).To(Equal(tgGateRevision(tgGateFailed, tgGateCommitA)))
	})

	It("rolls addon changes immediately when spec.upgrade is unset and records the applied hash", func() {
		ctx := context.Background()
		createInstance(ctx, tgGateNoSpec, nil)
		tgGateCreateAddon(ctx, tgGateNoSpec)
		tgGateSetCommit(ctx, tgGateNoSpec, tgGateCommitA)
		tgGateReconcile(ctx, tgGateNoSpec)
		Expect(fetchInstance(ctx, tgGateNoSpec).Status.AppliedAddonsHash).To(Equal(tgGateRevision(tgGateNoSpec, tgGateCommitA)))

		tgGateSetCommit(ctx, tgGateNoSpec, tgGateCommitB)
		tgGateReconcile(ctx, tgGateNoSpec)

		Expect(tgGatePodAddonsHash(ctx, tgGateNoSpec)).To(Equal(tgGateRevision(tgGateNoSpec, tgGateCommitB)))
		instance := fetchInstance(ctx, tgGateNoSpec)
		Expect(instance.Status.AppliedAddonsHash).To(Equal(tgGateRevision(tgGateNoSpec, tgGateCommitB)))
		Expect(instance.Status.ActiveUpgrade).To(BeEmpty())

		upgrades := &odoov1.OdooUpgradeList{}
		Expect(k8sClient.List(ctx, upgrades, client.InNamespace(testNamespace))).To(Succeed())
		for _, upgrade := range upgrades.Items {
			Expect(upgrade.Spec.InstanceRef.Name).NotTo(Equal(tgGateNoSpec))
		}
	})
})
