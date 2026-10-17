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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	tiEnvFlow      = "ti-env-flow"
	tiEnvRetry     = "ti-env-retry"
	tiEnvPlain     = "ti-env-plain"
	tiEnvAdopt     = "ti-env-adopt"
	tiEnvMissing   = "ti-env-missing"
	tiEnvLate      = "ti-env-late"
	tiEnvSecret    = "ti-env-admin-pw"
	tiEnvModule    = "base"
	tiEnvDatabase  = "odoo_init"
	tiEnvTimeout   = int64(900)
	tiEnvLiveImage = "odoo:19.0"
	tiEnvNoSecret  = "ti-env-does-not-exist"
)

// tiEnvInitName returns the name of the initialisation OdooUpgrade for an instance.
func tiEnvInitName(instanceName string) string {
	return initUpgradeName(instanceName)
}

// tiEnvInitSpec is the initialize block used by every test; the password Secret exists.
func tiEnvInitSpec(i *odoov1.OdooInstance) {
	i.Spec.Postgres.Database = tiEnvDatabase
	i.Spec.Initialize = &odoov1.InitializeSpec{
		Modules:                 []string{tiEnvModule},
		AdminUserPasswordSecret: tiEnvSecret,
		TimeoutSeconds:          int64Ptr(tiEnvTimeout),
	}
}

// tiEnvCreateSecret makes the admin password Secret the initialisation needs.
func tiEnvCreateSecret(ctx context.Context, name string) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Data:       map[string][]byte{secretKeyPassword: []byte("not-a-real-password")},
	}
	Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, secret))).To(Succeed())
	DeferCleanup(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), secret))).To(Succeed())
	})
}

// tiEnvReconcile runs one pass and returns its result and error.
func tiEnvReconcile(ctx context.Context, name string) (reconcile.Result, error) {
	reconciler := &OdooInstanceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	return reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace},
	})
}

// tiEnvMustReconcile runs one pass that must succeed.
func tiEnvMustReconcile(ctx context.Context, name string) reconcile.Result {
	result, err := tiEnvReconcile(ctx, name)
	Expect(err).NotTo(HaveOccurred())
	return result
}

// tiEnvSetUpgradeStatus plays the OdooUpgrade controller: it sets the phase and message.
func tiEnvSetUpgradeStatus(ctx context.Context, name, phase, message string) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		up := &odoov1.OdooUpgrade{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, up); err != nil {
			return err
		}
		up.Status.Phase = phase
		up.Status.Message = message
		return k8sClient.Status().Update(ctx, up)
	})).To(Succeed())
}

// tiEnvDeploymentReplicas returns spec.replicas of the instance's Deployment.
func tiEnvDeploymentReplicas(ctx context.Context, name string) int32 {
	deploy := fetchDeployment(ctx, name)
	Expect(deploy.Spec.Replicas).NotTo(BeNil())
	return *deploy.Spec.Replicas
}

var _ = Describe("OdooInstance database initialisation", func() {
	ctx := context.Background()

	It("holds the Deployment at zero until the init OdooUpgrade succeeds", func() {
		tiEnvCreateSecret(ctx, tiEnvSecret)
		createInstance(ctx, tiEnvFlow, tiEnvInitSpec)
		initName := tiEnvInitName(tiEnvFlow)
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(),
				&odoov1.OdooUpgrade{ObjectMeta: metav1.ObjectMeta{Name: initName, Namespace: testNamespace}}))).To(Succeed())
		})

		tiEnvMustReconcile(ctx, tiEnvFlow)

		upgrade := tgGateFetchUpgrade(ctx, initName)
		Expect(upgrade.Spec.InstanceRef.Name).To(Equal(tiEnvFlow))
		Expect(upgrade.Spec.InstallModules).To(Equal([]string{tiEnvModule}))
		Expect(upgrade.Spec.Modules).To(BeEmpty())
		Expect(upgrade.Spec.WithDemo).To(BeFalse())
		Expect(upgrade.Spec.AdminUserPasswordSecret).To(Equal(tiEnvSecret))
		Expect(upgrade.Spec.TimeoutSeconds).NotTo(BeNil())
		Expect(*upgrade.Spec.TimeoutSeconds).To(Equal(tiEnvTimeout))
		Expect(upgrade.Labels).To(HaveKeyWithValue(gateLabelInstance, tiEnvFlow))
		Expect(metav1.IsControlledBy(upgrade, fetchInstance(ctx, tiEnvFlow))).To(BeTrue())

		Expect(tiEnvDeploymentReplicas(ctx, tiEnvFlow)).To(BeZero())
		instance := fetchInstance(ctx, tiEnvFlow)
		Expect(instance.Status.Phase).To(Equal(phaseInitializing))
		Expect(instance.Status.Initialized).To(BeFalse())
		Expect(instance.Status.InitUpgrade).To(Equal(initName))
		Expect(instance.Status.Ready).To(BeFalse())

		// A repeat pass reuses the upgrade and keeps holding.
		tiEnvMustReconcile(ctx, tiEnvFlow)
		Expect(tgGateFetchUpgrade(ctx, initName).UID).To(Equal(upgrade.UID))
		Expect(tiEnvDeploymentReplicas(ctx, tiEnvFlow)).To(BeZero())

		tiEnvSetUpgradeStatus(ctx, initName, odoov1.UpgradePhaseSucceeded, "")
		tiEnvMustReconcile(ctx, tiEnvFlow)

		instance = fetchInstance(ctx, tiEnvFlow)
		Expect(instance.Status.Initialized).To(BeTrue())
		Expect(instance.Status.InitUpgrade).To(BeEmpty())
		Expect(instance.Status.Phase).NotTo(Equal(phaseInitializing))
		Expect(tiEnvDeploymentReplicas(ctx, tiEnvFlow)).To(Equal(int32(1)))
	})

	It("reports InitializationFailed with replicas at zero, then retries after the failed upgrade is deleted", func() {
		tiEnvCreateSecret(ctx, tiEnvSecret)
		createInstance(ctx, tiEnvRetry, tiEnvInitSpec)
		initName := tiEnvInitName(tiEnvRetry)
		tiEnvMustReconcile(ctx, tiEnvRetry)

		tiEnvSetUpgradeStatus(ctx, initName, odoov1.UpgradePhaseFailed, "module not found")
		result := tiEnvMustReconcile(ctx, tiEnvRetry)
		Expect(result.RequeueAfter).To(Equal(initFailedRequeue))

		instance := fetchInstance(ctx, tiEnvRetry)
		Expect(instance.Status.Phase).To(Equal(phaseInitFailed))
		Expect(instance.Status.Ready).To(BeFalse())
		Expect(instance.Status.Initialized).To(BeFalse())
		Expect(instance.Status.InitUpgrade).To(Equal(initName))
		Expect(instance.Status.Message).To(ContainSubstring("module not found"))
		Expect(tiEnvDeploymentReplicas(ctx, tiEnvRetry)).To(BeZero())
		degraded := meta.FindStatusCondition(instance.Status.Conditions, odoov1.ConditionTypeDegraded)
		Expect(degraded.Status).To(Equal(condTrue))
		Expect(degraded.Reason).To(Equal(odoov1.ReasonInitializationFailed))

		// Retry: delete the failed upgrade; the next pass creates a fresh one.
		Expect(k8sClient.Delete(ctx, tgGateFetchUpgrade(ctx, initName))).To(Succeed())
		tiEnvMustReconcile(ctx, tiEnvRetry)
		recreated := tgGateFetchUpgrade(ctx, initName)
		Expect(recreated.Status.Phase).NotTo(Equal(odoov1.UpgradePhaseFailed))
		Expect(fetchInstance(ctx, tiEnvRetry).Status.Phase).To(Equal(phaseInitializing))
		Expect(tiEnvDeploymentReplicas(ctx, tiEnvRetry)).To(BeZero())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(),
				&odoov1.OdooUpgrade{ObjectMeta: metav1.ObjectMeta{Name: initName, Namespace: testNamespace}}))).To(Succeed())
		})
	})

	It("leaves an instance without spec.initialize unaffected", func() {
		createInstance(ctx, tiEnvPlain, func(i *odoov1.OdooInstance) {
			i.Spec.Postgres.Database = tiEnvDatabase
		})
		tiEnvMustReconcile(ctx, tiEnvPlain)

		instance := fetchInstance(ctx, tiEnvPlain)
		Expect(instance.Status.Initialized).To(BeFalse())
		Expect(instance.Status.InitUpgrade).To(BeEmpty())
		Expect(instance.Status.Phase).To(Equal(phaseCreating))
		Expect(tiEnvDeploymentReplicas(ctx, tiEnvPlain)).To(Equal(int32(1)))

		err := k8sClient.Get(ctx, types.NamespacedName{Name: tiEnvInitName(tiEnvPlain), Namespace: testNamespace},
			&odoov1.OdooUpgrade{})
		Expect(err).To(HaveOccurred())
	})

	It("does not re-initialise a Deployment that already has ready pods", func() {
		tiEnvCreateSecret(ctx, tiEnvSecret)
		createInstance(ctx, tiEnvAdopt, tiEnvInitSpec)
		live := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: tiEnvAdopt, Namespace: testNamespace},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: selectorLabels(tiEnvAdopt)},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: selectorLabels(tiEnvAdopt)},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "odoo", Image: tiEnvLiveImage}}},
				},
			},
		}
		Expect(k8sClient.Create(ctx, live)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), live))).To(Succeed())
		})
		live.Status.Replicas = 1
		live.Status.ReadyReplicas = 1
		Expect(k8sClient.Status().Update(ctx, live)).To(Succeed())

		tiEnvMustReconcile(ctx, tiEnvAdopt)

		instance := fetchInstance(ctx, tiEnvAdopt)
		Expect(instance.Status.Initialized).To(BeTrue())
		Expect(instance.Status.InitUpgrade).To(BeEmpty())
		Expect(tiEnvDeploymentReplicas(ctx, tiEnvAdopt)).To(Equal(int32(1)))
		err := k8sClient.Get(ctx, types.NamespacedName{Name: tiEnvInitName(tiEnvAdopt), Namespace: testNamespace},
			&odoov1.OdooUpgrade{})
		Expect(err).To(HaveOccurred(), "no initialisation upgrade may run against a live database")
	})

	It("fails before creating the upgrade when the admin password Secret is missing", func() {
		createInstance(ctx, tiEnvMissing, func(i *odoov1.OdooInstance) {
			tiEnvInitSpec(i)
			i.Spec.Initialize.AdminUserPasswordSecret = tiEnvNoSecret
		})
		_, err := tiEnvReconcile(ctx, tiEnvMissing)
		Expect(err).To(HaveOccurred())
		Expect(reconcileErrorReason(err)).To(Equal(odoov1.ReasonMissingSecret))
		err = k8sClient.Get(ctx, types.NamespacedName{Name: tiEnvInitName(tiEnvMissing), Namespace: testNamespace},
			&odoov1.OdooUpgrade{})
		Expect(err).To(HaveOccurred())
	})

	It("rejects adding spec.initialize to an existing instance", func() {
		createInstance(ctx, tiEnvLate, nil)
		instance := fetchInstance(ctx, tiEnvLate)
		instance.Spec.Initialize = &odoov1.InitializeSpec{
			Modules:                 []string{tiEnvModule},
			AdminUserPasswordSecret: tiEnvSecret,
		}
		err := k8sClient.Update(ctx, instance)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("re-initialise a live database"))
	})

	It("rejects an initialize block without an admin password Secret", func() {
		instance := &odoov1.OdooInstance{
			ObjectMeta: metav1.ObjectMeta{Name: "ti-env-nopw", Namespace: testNamespace},
			Spec: odoov1.OdooInstanceSpec{
				Initialize: &odoov1.InitializeSpec{Modules: []string{tiEnvModule}},
			},
		}
		Expect(k8sClient.Create(ctx, instance)).NotTo(Succeed())
	})

})
