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
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

// Each spec uses its own instance and Secret names: envtest runs no garbage
// collector, so leftover child objects must never collide with another spec.
const (
	hardeningDefault   = "hardening-default"
	hardeningRetain    = "hardening-retain-false"
	hardeningRWX       = "hardening-rwx"
	hardeningStale     = "hardening-stale-owner"
	hardeningSingle    = "hardening-single"
	hardeningMulti     = "hardening-multi"
	hardeningConfig    = "hardening-config"
	hardeningAdmin     = "hardening-admin"
	hardeningMissing   = "hardening-missing"
	hardeningRotate    = "hardening-rotate"
	hardeningAddon     = "hardening-addon"
	hardeningPDB       = "hardening-pdb"
	hardeningDelete    = "hardening-delete"
	hardeningBlocked   = "hardening-blocked"
	hardeningAddonName = "hardening-addon-theme"
	hardeningSpec      = "hardening-spec"
	hardeningCM        = "hardening-cm"
	hardeningSvc       = "hardening-svc"
	hardeningIdem      = "hardening-idempotent"

	secretAdminName   = "hardening-admin-secret"
	secretPGName      = "hardening-pg-secret"
	secretMissingName = "hardening-missing-secret"
)

func createSecret(ctx context.Context, name string, data map[string][]byte) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Data:       data,
	}
	Expect(k8sClient.Create(ctx, secret)).To(Succeed())
	DeferCleanup(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), secret))).To(Succeed())
	})
}

func createInstance(ctx context.Context, name string, mutate func(*odoov1.OdooInstance)) {
	instance := &odoov1.OdooInstance{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
	if mutate != nil {
		mutate(instance)
	}
	Expect(k8sClient.Create(ctx, instance)).To(Succeed())
	DeferCleanup(func() {
		removeInstance(context.Background(), name)
	})
}

// removeInstance clears finalizers (the reconciler is not running) and deletes the instance.
func removeInstance(ctx context.Context, name string) {
	instance := &odoov1.OdooInstance{}
	err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, instance)
	if errors.IsNotFound(err) {
		return
	}
	Expect(err).NotTo(HaveOccurred())
	instance.Finalizers = nil
	Expect(client.IgnoreNotFound(k8sClient.Update(ctx, instance))).To(Succeed())
	Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, instance))).To(Succeed())
}

func reconcileInstance(ctx context.Context, name string) error {
	reconciler := &OdooInstanceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	_, err := reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: name, Namespace: testNamespace},
	})
	return err
}

func mustReconcile(ctx context.Context, name string) {
	Expect(reconcileInstance(ctx, name)).To(Succeed())
}

func fetchInstance(ctx context.Context, name string) *odoov1.OdooInstance {
	instance := &odoov1.OdooInstance{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, instance)).To(Succeed())
	return instance
}

func fetchPVC(ctx context.Context, name string) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, pvc)).To(Succeed())
	return pvc
}

func fetchDeployment(ctx context.Context, name string) *appsv1.Deployment {
	deploy := &appsv1.Deployment{}
	Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: testNamespace}, deploy)).To(Succeed())
	return deploy
}

func odooContainer(deploy *appsv1.Deployment) corev1.Container {
	containers := deploy.Spec.Template.Spec.Containers
	Expect(containers).To(HaveLen(1))
	return containers[0]
}

var _ = Describe("OdooInstance production hardening", func() {
	Context("PVC retention and access modes", func() {
		It("keeps the data PVC unowned by default and owns the addons PVC", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningDefault, nil)
			mustReconcile(ctx, hardeningDefault)

			instance := fetchInstance(ctx, hardeningDefault)
			addons := fetchPVC(ctx, addonsPVCName(hardeningDefault))
			Expect(metav1.IsControlledBy(addons, instance)).To(BeTrue())

			data := fetchPVC(ctx, dataPVCName(hardeningDefault))
			Expect(data.OwnerReferences).To(BeEmpty())
			Expect(data.Spec.AccessModes).To(Equal([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}))
			Expect(data.Spec.StorageClassName).To(BeNil(), "storage class must be left to the cluster default")
			Expect(addons.Spec.StorageClassName).To(BeNil())
		})

		It("owns the data PVC when retainOnDelete is false", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningRetain, func(i *odoov1.OdooInstance) {
				no := false
				i.Spec.Data.RetainOnDelete = &no
			})
			mustReconcile(ctx, hardeningRetain)

			instance := fetchInstance(ctx, hardeningRetain)
			data := fetchPVC(ctx, dataPVCName(hardeningRetain))
			Expect(metav1.IsControlledBy(data, instance)).To(BeTrue())
		})

		It("honors ReadWriteMany on both PVCs", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningRWX, func(i *odoov1.OdooInstance) {
				i.Spec.Addons.AccessMode = corev1.ReadWriteMany
				i.Spec.Data.AccessMode = corev1.ReadWriteMany
			})
			mustReconcile(ctx, hardeningRWX)

			rwx := []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
			Expect(fetchPVC(ctx, addonsPVCName(hardeningRWX)).Spec.AccessModes).To(Equal(rwx))
			Expect(fetchPVC(ctx, dataPVCName(hardeningRWX)).Spec.AccessModes).To(Equal(rwx))
		})

		It("removes a stale owner reference from a retained data PVC", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningStale, nil)
			mustReconcile(ctx, hardeningStale)

			// Simulate a PVC created by an earlier operator version that owned the filestore.
			instance := fetchInstance(ctx, hardeningStale)
			data := fetchPVC(ctx, dataPVCName(hardeningStale))
			Expect(controllerutil.SetControllerReference(instance, data, k8sClient.Scheme())).To(Succeed())
			Expect(k8sClient.Update(ctx, data)).To(Succeed())

			mustReconcile(ctx, hardeningStale)
			Expect(fetchPVC(ctx, dataPVCName(hardeningStale)).OwnerReferences).To(BeEmpty())
		})
	})

	Context("Deployment contents", func() {
		It("uses Recreate for one replica on an RWO data volume", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningSingle, nil)
			mustReconcile(ctx, hardeningSingle)

			strategy := fetchDeployment(ctx, hardeningSingle).Spec.Strategy
			Expect(strategy.Type).To(Equal(appsv1.RecreateDeploymentStrategyType))
			Expect(strategy.RollingUpdate).To(BeNil())
		})

		It("uses a zero-unavailable rolling update for ReadWriteMany replicas", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningMulti, func(i *odoov1.OdooInstance) {
				i.Spec.Replicas = 2
				i.Spec.Addons.AccessMode = corev1.ReadWriteMany
				i.Spec.Data.AccessMode = corev1.ReadWriteMany
			})
			mustReconcile(ctx, hardeningMulti)

			strategy := fetchDeployment(ctx, hardeningMulti).Spec.Strategy
			Expect(strategy.Type).To(Equal(appsv1.RollingUpdateDeploymentStrategyType))
			Expect(strategy.RollingUpdate).NotTo(BeNil())
			Expect(*strategy.RollingUpdate.MaxUnavailable).To(Equal(intstr.FromInt32(0)))
			Expect(*strategy.RollingUpdate.MaxSurge).To(Equal(intstr.FromInt32(1)))
		})

		It("renders probes, ports, labels and pod hardening", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningSpec, nil)
			mustReconcile(ctx, hardeningSpec)

			deploy := fetchDeployment(ctx, hardeningSpec)
			podSpec := deploy.Spec.Template.Spec
			container := odooContainer(deploy)

			Expect(container.StartupProbe).NotTo(BeNil())
			Expect(container.StartupProbe.FailureThreshold).To(Equal(int32(30)))
			Expect(container.StartupProbe.PeriodSeconds).To(Equal(int32(10)))
			Expect(container.ReadinessProbe.PeriodSeconds).To(Equal(int32(10)))
			Expect(container.LivenessProbe.PeriodSeconds).To(Equal(int32(20)))
			Expect(container.LivenessProbe.FailureThreshold).To(Equal(int32(3)))
			for _, probe := range []*corev1.Probe{container.StartupProbe, container.ReadinessProbe, container.LivenessProbe} {
				Expect(probe.HTTPGet).NotTo(BeNil())
				Expect(probe.HTTPGet.Path).To(Equal(healthPath))
				Expect(probe.HTTPGet.Port).To(Equal(intstr.FromInt32(portOdoo)))
			}

			Expect(container.Ports).To(ContainElements(
				corev1.ContainerPort{Name: portNameOdoo, ContainerPort: 8069, Protocol: corev1.ProtocolTCP},
				corev1.ContainerPort{Name: portNameLongpolling, ContainerPort: 8072, Protocol: corev1.ProtocolTCP},
			))

			Expect(podSpec.TerminationGracePeriodSeconds).NotTo(BeNil())
			Expect(*podSpec.TerminationGracePeriodSeconds).To(Equal(int64(60)))
			Expect(podSpec.AutomountServiceAccountToken).NotTo(BeNil())
			Expect(*podSpec.AutomountServiceAccountToken).To(BeFalse())
			Expect(container.SecurityContext.ReadOnlyRootFilesystem).NotTo(BeNil())
			Expect(*container.SecurityContext.ReadOnlyRootFilesystem).To(BeTrue())
			Expect(container.VolumeMounts).To(ContainElement(corev1.VolumeMount{Name: volumeTmp, MountPath: mountPathTmp}))

			Expect(deploy.Labels).To(HaveKeyWithValue(labelAppName, appOdoo))
			Expect(deploy.Labels).To(HaveKeyWithValue(labelAppInstance, hardeningSpec))
			Expect(deploy.Labels).To(HaveKeyWithValue(labelManagedBy, managedByOperator))
			Expect(deploy.Spec.Selector.MatchLabels).To(Equal(map[string]string{
				labelApp:      appOdoo,
				labelInstance: hardeningSpec,
			}))
		})

		It("stamps rollout hashes and changes the config hash when config changes", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningConfig, nil)
			mustReconcile(ctx, hardeningConfig)
			before := fetchDeployment(ctx, hardeningConfig).Spec.Template.Annotations
			Expect(before).To(HaveKey(annotationConfigHash))
			Expect(before).To(HaveKey(annotationSecretsHash))
			Expect(before).To(HaveKey(annotationAddonsHash))

			instance := fetchInstance(ctx, hardeningConfig)
			instance.Spec.Config = map[string]string{testKeyWorkers: "3"}
			Expect(k8sClient.Update(ctx, instance)).To(Succeed())
			mustReconcile(ctx, hardeningConfig)

			after := fetchDeployment(ctx, hardeningConfig).Spec.Template.Annotations
			Expect(after[annotationConfigHash]).NotTo(Equal(before[annotationConfigHash]))

			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configMapName(hardeningConfig), Namespace: testNamespace}, cm)).To(Succeed())
			Expect(cm.Data[confFileName]).To(ContainSubstring("workers = 3\n"))
		})

		It("renders the admin password into an in-memory config through an init container", func() {
			ctx := context.Background()
			createSecret(ctx, secretAdminName, map[string][]byte{secretKeyPassword: []byte("master")})
			createInstance(ctx, hardeningAdmin, func(i *odoov1.OdooInstance) {
				i.Spec.AdminPasswordSecret = secretAdminName
			})
			mustReconcile(ctx, hardeningAdmin)

			deploy := fetchDeployment(ctx, hardeningAdmin)
			container := odooContainer(deploy)
			Expect(container.Args).To(Equal([]string{upgradeFlagConfig, renderedConfPath}))
			Expect(container.Env).NotTo(ContainElement(HaveField("Name", envAdminPasswd)),
				"the web container must not receive the master password")

			podSpec := deploy.Spec.Template.Spec
			Expect(podSpec.InitContainers).To(HaveLen(1))
			Expect(podSpec.InitContainers[0].Env).To(ContainElement(corev1.EnvVar{
				Name: envAdminPasswd,
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretAdminName},
					Key:                  secretKeyPassword,
				}},
			}))
			Expect(podSpec.Volumes).To(ContainElement(HaveField("Name", volumeRendered)))

			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configMapName(hardeningAdmin), Namespace: testNamespace}, cm)).To(Succeed())
			Expect(cm.Data[confFileName]).NotTo(ContainSubstring("master"), "admin password must never reach the ConfigMap")
		})

		It("fails with a clear error when a referenced Secret is missing", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningMissing, func(i *odoov1.OdooInstance) {
				i.Spec.Postgres.PasswordSecret = secretMissingName
			})
			err := reconcileInstance(ctx, hardeningMissing)
			Expect(err).To(MatchError(ContainSubstring(secretMissingName)))
		})

		It("changes the secrets hash when a referenced Secret is rotated", func() {
			ctx := context.Background()
			createSecret(ctx, secretPGName, map[string][]byte{secretKeyPassword: []byte("one")})
			createInstance(ctx, hardeningRotate, func(i *odoov1.OdooInstance) {
				i.Spec.Postgres.PasswordSecret = secretPGName
			})
			mustReconcile(ctx, hardeningRotate)
			before := fetchDeployment(ctx, hardeningRotate).Spec.Template.Annotations[annotationSecretsHash]

			secret := &corev1.Secret{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secretPGName, Namespace: testNamespace}, secret)).To(Succeed())
			secret.Data[secretKeyPassword] = []byte("two")
			Expect(k8sClient.Update(ctx, secret)).To(Succeed())

			mustReconcile(ctx, hardeningRotate)
			after := fetchDeployment(ctx, hardeningRotate).Spec.Template.Annotations[annotationSecretsHash]
			Expect(after).NotTo(Equal(before))
		})

		It("changes the addons hash when a ready addon re-syncs to a new commit", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningAddon, nil)

			addon := &odoov1.OdooAddon{
				ObjectMeta: metav1.ObjectMeta{Name: hardeningAddonName, Namespace: testNamespace},
				Spec: odoov1.OdooAddonSpec{
					GitUrl:      "https://example.com/acme/odoo-addons.git",
					InstanceRef: odoov1.OdooInstanceRef{Name: hardeningAddon},
				},
			}
			Expect(k8sClient.Create(ctx, addon)).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(context.Background(), addon))).To(Succeed())
			})

			setAddonCommit := func(commit string) {
				current := &odoov1.OdooAddon{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hardeningAddonName, Namespace: testNamespace}, current)).To(Succeed())
				current.Status.Ready = true
				current.Status.ClonedCommit = commit
				Expect(k8sClient.Status().Update(ctx, current)).To(Succeed())
			}

			setAddonCommit("abc1234")
			mustReconcile(ctx, hardeningAddon)
			Expect(fetchDeployment(ctx, hardeningAddon).Spec.Template.Annotations[annotationAddonsHash]).
				To(Equal(sha256Hex(hardeningAddonName + "=abc1234")))
			Expect(fetchInstance(ctx, hardeningAddon).Status.AddonPaths).
				To(ContainElement(addonsMountPath + "/" + hardeningAddonName))

			setAddonCommit("def5678")
			mustReconcile(ctx, hardeningAddon)
			Expect(fetchDeployment(ctx, hardeningAddon).Spec.Template.Annotations[annotationAddonsHash]).
				To(Equal(sha256Hex(hardeningAddonName + "=def5678")))
		})
	})

	Context("PodDisruptionBudget", func() {
		It("exists for multiple replicas and is deleted when scaled to one", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningPDB, func(i *odoov1.OdooInstance) {
				i.Spec.Replicas = 2
				i.Spec.Addons.AccessMode = corev1.ReadWriteMany
				i.Spec.Data.AccessMode = corev1.ReadWriteMany
			})
			mustReconcile(ctx, hardeningPDB)

			pdb := &policyv1.PodDisruptionBudget{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hardeningPDB, Namespace: testNamespace}, pdb)).To(Succeed())
			Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
			Expect(*pdb.Spec.MaxUnavailable).To(Equal(intstr.FromInt32(1)))
			Expect(pdb.Spec.Selector.MatchLabels).To(HaveKeyWithValue(labelApp, appOdoo))
			Expect(metav1.IsControlledBy(pdb, fetchInstance(ctx, hardeningPDB))).To(BeTrue())

			instance := fetchInstance(ctx, hardeningPDB)
			instance.Spec.Replicas = 1
			Expect(k8sClient.Update(ctx, instance)).To(Succeed())
			mustReconcile(ctx, hardeningPDB)

			err := k8sClient.Get(ctx, types.NamespacedName{Name: hardeningPDB, Namespace: testNamespace}, pdb)
			Expect(errors.IsNotFound(err)).To(BeTrue())
		})
	})

	Context("configuration, Service and lifecycle", func() {
		It("renders production defaults into the ConfigMap", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningCM, nil)
			mustReconcile(ctx, hardeningCM)

			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configMapName(hardeningCM), Namespace: testNamespace}, cm)).To(Succeed())
			Expect(cm.Data[confFileName]).To(ContainSubstring("proxy_mode = True\n"))
			Expect(cm.Data[confFileName]).To(ContainSubstring("list_db = False\n"))
		})

		It("rejects blocked config keys during reconcile", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningBlocked, func(i *odoov1.OdooInstance) {
				i.Spec.Config = map[string]string{"admin_passwd": "nope"}
			})
			Expect(reconcileInstance(ctx, hardeningBlocked)).To(MatchError(ContainSubstring(errNotPermitted)))
		})

		It("does not rewrite child objects when nothing changed", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningIdem, nil)
			mustReconcile(ctx, hardeningIdem)

			resourceVersions := func() []string {
				deploy := fetchDeployment(ctx, hardeningIdem)
				svc := &corev1.Service{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hardeningIdem, Namespace: testNamespace}, svc)).To(Succeed())
				cm := &corev1.ConfigMap{}
				Expect(k8sClient.Get(ctx, types.NamespacedName{Name: configMapName(hardeningIdem), Namespace: testNamespace}, cm)).To(Succeed())
				return []string{deploy.ResourceVersion, svc.ResourceVersion, cm.ResourceVersion}
			}
			before := resourceVersions()
			mustReconcile(ctx, hardeningIdem)
			Expect(resourceVersions()).To(Equal(before), "a steady-state reconcile must not generate watch events")
		})

		It("exposes the longpolling port on the Service", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningSvc, nil)
			mustReconcile(ctx, hardeningSvc)

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: hardeningSvc, Namespace: testNamespace}, svc)).To(Succeed())
			Expect(svc.Spec.Ports).To(ContainElement(HaveField("Name", portNameOdoo)))
			Expect(svc.Spec.Ports).To(ContainElement(HaveField("Name", portNameLongpolling)))
			Expect(svc.Spec.Ports).To(ContainElement(HaveField("Port", portLongpolling)))
		})

		It("keeps the data PVC after the OdooInstance is deleted", func() {
			ctx := context.Background()
			createInstance(ctx, hardeningDelete, nil)
			mustReconcile(ctx, hardeningDelete)

			instance := fetchInstance(ctx, hardeningDelete)
			Expect(k8sClient.Delete(ctx, instance)).To(Succeed())
			// The finalizer is released without deleting any child objects.
			mustReconcile(ctx, hardeningDelete)

			err := k8sClient.Get(ctx, types.NamespacedName{Name: hardeningDelete, Namespace: testNamespace}, instance)
			Expect(errors.IsNotFound(err)).To(BeTrue())
			data := fetchPVC(ctx, dataPVCName(hardeningDelete))
			Expect(data.OwnerReferences).To(BeEmpty())
		})
	})
})
