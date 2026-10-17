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
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	cronEnvEnable  = "cron-env-enable"
	cronEnvDisable = "cron-env-disable"
	cronEnvStatus  = "cron-env-status"
	cronEnvReject  = "cron-env-reject"
	cronEnvAbsent  = "cron-env-absent"
	cronEnvInit    = "cron-env-init"
	cronEnvIso     = "cron-env-iso"
	cronEnvSecret  = "cron-env-admin-pw"
	cronEnvThreads = int32(3)
)

// cronEnvEnabled is an enabled cron spec with cronEnvThreads workers.
func cronEnvEnabled() *odoov1.CronSpec {
	return &odoov1.CronSpec{Enabled: true, MaxCronThreads: int32Ptr(cronEnvThreads)}
}

// cronEnvFetchCron returns the "<name>-cron" Deployment of an instance.
func cronEnvFetchCron(ctx context.Context, name string) *appsv1.Deployment {
	return fetchDeployment(ctx, cronDeploymentName(name))
}

// cronEnvCondition returns the CronReady condition, nil when absent.
func cronEnvCondition(ctx context.Context, name string) *metav1.Condition {
	return meta.FindStatusCondition(fetchInstance(ctx, name).Status.Conditions, odoov1.ConditionTypeCronReady)
}

// cronEnvConfigMap returns the rendered odoo.conf of an instance.
func cronEnvConfigMap(ctx context.Context, name string) string {
	cm := &corev1.ConfigMap{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configMapName(name), Namespace: testNamespace}, cm)).To(Succeed())
	return cm.Data[confFileName]
}

// cronEnvSetDeploymentStatus plays the Deployment controller for the cron Deployment.
func cronEnvSetDeploymentStatus(ctx context.Context, deployName string, replicas, ready int32) {
	Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deploy := &appsv1.Deployment{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: deployName, Namespace: testNamespace}, deploy); err != nil {
			return err
		}
		deploy.Status.Replicas = replicas
		deploy.Status.ReadyReplicas = ready
		return k8sClient.Status().Update(ctx, deploy)
	})).To(Succeed())
}

var _ = Describe("OdooInstance dedicated cron Deployment", func() {
	Context("when spec.cron is enabled", func() {
		It("creates <name>-cron with cron args and no web surface, and turns cron off in the web pods", func() {
			ctx := context.Background()
			createInstance(ctx, cronEnvEnable, func(i *odoov1.OdooInstance) {
				i.Spec.Cron = cronEnvEnabled()
			})
			mustReconcile(ctx, cronEnvEnable)

			instance := fetchInstance(ctx, cronEnvEnable)
			cron := cronEnvFetchCron(ctx, cronEnvEnable)
			Expect(metav1.IsControlledBy(cron, instance)).To(BeTrue())
			Expect(cron.Spec.Replicas).NotTo(BeNil())
			Expect(*cron.Spec.Replicas).To(Equal(int32(1)))
			Expect(cron.Spec.Strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
			Expect(cron.Spec.Selector.MatchLabels).To(Equal(cronSelectorLabels(cronEnvEnable)))

			container := odooContainer(cron)
			Expect(container.Args).To(ContainElements(upgradeFlagConfig, odooConfPath, "--no-http", "--max-cron-threads=3"))
			Expect(container.Ports).To(BeEmpty())
			Expect(container.StartupProbe).To(BeNil())
			Expect(container.ReadinessProbe).To(BeNil())
			Expect(container.LivenessProbe).To(BeNil())

			// Same volumes and rollout hashes as the web pods, so both roll together.
			web := fetchDeployment(ctx, cronEnvEnable)
			Expect(cron.Spec.Template.Annotations).To(Equal(web.Spec.Template.Annotations))
			Expect(cron.Spec.Template.Spec.Volumes).To(HaveLen(len(web.Spec.Template.Spec.Volumes)))

			// The default volumes are ReadWriteOnce, so the cron pod must share a node with a web pod.
			Expect(cron.Spec.Template.Spec.Affinity).NotTo(BeNil())
			Expect(cron.Spec.Template.Spec.Affinity.PodAffinity).NotTo(BeNil())

			// The web Service must never select a cron pod.
			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cronEnvEnable, Namespace: testNamespace}, svc)).To(Succeed())
			Expect(labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(cron.Spec.Template.Labels))).To(BeFalse())

			// The web pods run no cron workers.
			Expect(cronEnvConfigMap(ctx, cronEnvEnable)).To(ContainSubstring("max_cron_threads = 0\n"))
		})

		It("holds the cron pod at zero replicas while the database is initialising", func() {
			ctx := context.Background()
			tiEnvCreateSecret(ctx, cronEnvSecret)
			createInstance(ctx, cronEnvInit, func(i *odoov1.OdooInstance) {
				tiEnvInitSpec(i)
				i.Spec.Initialize.AdminUserPasswordSecret = cronEnvSecret
				i.Spec.Cron = cronEnvEnabled()
			})
			tiEnvMustReconcile(ctx, cronEnvInit)

			Expect(*cronEnvFetchCron(ctx, cronEnvInit).Spec.Replicas).To(Equal(int32(0)))
			Expect(tiEnvDeploymentReplicas(ctx, cronEnvInit)).To(Equal(int32(0)))
			condition := cronEnvCondition(ctx, cronEnvInit)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(odoov1.ReasonCronPending))
		})

		It("refuses a max_cron_threads value in spec.config and creates no cron Deployment", func() {
			ctx := context.Background()
			createInstance(ctx, cronEnvReject, func(i *odoov1.OdooInstance) {
				i.Spec.Cron = cronEnvEnabled()
				i.Spec.Config = map[string]string{confKeyMaxCronThreads: "4"}
			})
			Expect(reconcileInstance(ctx, cronEnvReject)).To(HaveOccurred())

			instance := fetchInstance(ctx, cronEnvReject)
			degraded := meta.FindStatusCondition(instance.Status.Conditions, odoov1.ConditionTypeDegraded)
			Expect(degraded).NotTo(BeNil())
			Expect(degraded.Reason).To(Equal(odoov1.ReasonInvalidConfig))
			Expect(degraded.Message).To(ContainSubstring("cannot be set in spec.config"))

			err := k8sClient.Get(ctx, types.NamespacedName{Name: cronDeploymentName(cronEnvReject), Namespace: testNamespace},
				&appsv1.Deployment{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})

		It("reports CronReady from the cron Deployment status and leaves Ready to the web pods", func() {
			ctx := context.Background()
			createInstance(ctx, cronEnvStatus, func(i *odoov1.OdooInstance) {
				i.Spec.Cron = cronEnvEnabled()
			})
			mustReconcile(ctx, cronEnvStatus)

			condition := cronEnvCondition(ctx, cronEnvStatus)
			Expect(condition).NotTo(BeNil())
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(odoov1.ReasonCronPending))

			cronEnvSetDeploymentStatus(ctx, cronDeploymentName(cronEnvStatus), 1, 0)
			mustReconcile(ctx, cronEnvStatus)
			condition = cronEnvCondition(ctx, cronEnvStatus)
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(odoov1.ReasonCronNotReady))

			cronEnvSetDeploymentStatus(ctx, cronDeploymentName(cronEnvStatus), 1, 1)
			mustReconcile(ctx, cronEnvStatus)
			condition = cronEnvCondition(ctx, cronEnvStatus)
			Expect(condition.Status).To(Equal(metav1.ConditionTrue))
			Expect(condition.Reason).To(Equal(odoov1.ReasonCronRunning))

			// The web Deployment has no ready pods, so the instance is still not Ready.
			Expect(fetchInstance(ctx, cronEnvStatus).Status.Ready).To(BeFalse())
		})
	})

	Context("when spec.cron is not enabled", func() {
		It("creates no cron Deployment, no CronReady condition and no web override", func() {
			ctx := context.Background()
			createInstance(ctx, cronEnvAbsent, nil)
			mustReconcile(ctx, cronEnvAbsent)

			err := k8sClient.Get(ctx, types.NamespacedName{Name: cronDeploymentName(cronEnvAbsent), Namespace: testNamespace},
				&appsv1.Deployment{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
			Expect(cronEnvCondition(ctx, cronEnvAbsent)).To(BeNil())
			Expect(cronEnvConfigMap(ctx, cronEnvAbsent)).NotTo(ContainSubstring("max_cron_threads"))
		})

		It("removes the cron Deployment and the web override when cron is disabled", func() {
			ctx := context.Background()
			createInstance(ctx, cronEnvDisable, func(i *odoov1.OdooInstance) {
				i.Spec.Cron = cronEnvEnabled()
			})
			mustReconcile(ctx, cronEnvDisable)
			before := fetchDeployment(ctx, cronEnvDisable).Spec.Template.Annotations[annotationConfigHash]
			Expect(cronEnvCondition(ctx, cronEnvDisable)).NotTo(BeNil())

			instance := fetchInstance(ctx, cronEnvDisable)
			instance.Spec.Cron.Enabled = false
			Expect(k8sClient.Update(ctx, instance)).To(Succeed())
			mustReconcile(ctx, cronEnvDisable)

			err := k8sClient.Get(ctx, types.NamespacedName{Name: cronDeploymentName(cronEnvDisable), Namespace: testNamespace},
				&appsv1.Deployment{})
			Expect(errors.IsNotFound(err)).To(BeTrue())
			Expect(cronEnvCondition(ctx, cronEnvDisable)).To(BeNil())
			Expect(cronEnvConfigMap(ctx, cronEnvDisable)).NotTo(ContainSubstring("max_cron_threads"))

			// The config hash changes, so the web pods roll and run cron again.
			after := fetchDeployment(ctx, cronEnvDisable).Spec.Template.Annotations[annotationConfigHash]
			Expect(after).NotTo(Equal(before))
		})
	})

	Context("Service isolation", func() {
		It("gives the cron pod labels that the web Service selector does not match", func() {
			ctx := context.Background()
			createInstance(ctx, cronEnvIso, func(i *odoov1.OdooInstance) {
				i.Spec.Cron = cronEnvEnabled()
			})
			mustReconcile(ctx, cronEnvIso)

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cronEnvIso, Namespace: testNamespace}, svc)).To(Succeed())
			cronLabels := cronEnvFetchCron(ctx, cronEnvIso).Spec.Template.Labels
			Expect(cronLabels).To(HaveKeyWithValue(labelApp, appOdooCron))
			Expect(labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(cronLabels))).To(BeFalse())
		})
	})
})
