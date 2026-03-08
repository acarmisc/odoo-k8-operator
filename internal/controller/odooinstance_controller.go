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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	finalizerOdooInstance = "odoo.operator.io/finalizer"
	defaultOdooImage      = "odoo:17.0"
)

var odooLogger = logf.Log.WithName("controller_odooinstance")

type OdooInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *OdooInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	odooLogger.Info("Reconciling OdooInstance", "request", req.Name)

	instance := &odoov1.OdooInstance{}
	err := r.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		odooLogger.Error(err, "Failed to get OdooInstance")
		return ctrl.Result{}, err
	}

	if instance.ObjectMeta.DeletionTimestamp != nil {
		return ctrl.Result{}, r.handleFinalizer(ctx, instance)
	}

	if !controllerutil.ContainsFinalizer(instance, finalizerOdooInstance) {
		controllerutil.AddFinalizer(instance, finalizerOdooInstance)
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	result, err := r.reconcileOdooInstance(ctx, instance)
	if err != nil {
		odooLogger.Error(err, "Failed to reconcile OdooInstance")
		instance.Status.Phase = "Failed"
		instance.Status.Ready = false
		r.Status().Update(ctx, instance)
	}

	return result, err
}

func (r *OdooInstanceReconciler) reconcileOdooInstance(ctx context.Context, instance *odoov1.OdooInstance) (ctrl.Result, error) {
	instance.Status.Phase = "Creating"
	instance.Status.ObservedGeneration = instance.Generation

	if err := r.reconcilePVC(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileConfigMap(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileDeployment(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileService(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	instance.Status.Phase = "Running"
	instance.Status.Ready = true
	if err := r.Status().Update(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *OdooInstanceReconciler) reconcilePVC(ctx context.Context, instance *odoov1.OdooInstance) error {
	pvcName := fmt.Sprintf("%s-addons", instance.Name)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: instance.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pvc, func() error {
		if err := controllerutil.SetControllerReference(instance, pvc, r.Scheme); err != nil {
			return err
		}

		storageClass := instance.Spec.Addons.StorageClass
		if storageClass == nil {
			defaultClass := "standard"
			storageClass = &defaultClass
		}

		size := instance.Spec.Addons.Size
		if size == "" {
			size = "10Gi"
		}

		pvc.Spec = corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			VolumeMode:  nil,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(size),
				},
			},
			StorageClassName: storageClass,
		}
		return nil
	})

	return err
}

func (r *OdooInstanceReconciler) reconcileConfigMap(ctx context.Context, instance *odoov1.OdooInstance) error {
	cmName := fmt.Sprintf("%s-config", instance.Name)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: instance.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if err := controllerutil.SetControllerReference(instance, cm, r.Scheme); err != nil {
			return err
		}

		odooConfig := "[options]\n"
		odooConfig += "addons_path = /mnt/odoo/addons\n"

		for key, value := range instance.Spec.Config {
			odooConfig += fmt.Sprintf("%s = %s\n", key, value)
		}

		if instance.Spec.Postgres.Database != "" {
			odooConfig += fmt.Sprintf("db_host = %s\n", instance.Spec.Postgres.Host)
			odooConfig += fmt.Sprintf("db_port = %d\n", instance.Spec.Postgres.Port)
			odooConfig += fmt.Sprintf("db_name = %s\n", instance.Spec.Postgres.Database)
			odooConfig += fmt.Sprintf("db_user = %s\n", instance.Spec.Postgres.User)
			odooConfig += "db_password = ${DB_PASSWORD}\n"
		}

		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data["odoo.conf"] = odooConfig
		return nil
	})

	return err
}

func (r *OdooInstanceReconciler) reconcileDeployment(ctx context.Context, instance *odoov1.OdooInstance) error {
	deployName := instance.Name
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deployName,
			Namespace: instance.Namespace,
		},
	}

	image := instance.Spec.Image
	if image == "" {
		image = defaultOdooImage
	}

	replicas := instance.Spec.Replicas
	if replicas < 1 {
		replicas = 1
	}

	var envVars []corev1.EnvVar
	if instance.Spec.Postgres.PasswordSecret != "" {
		envVars = append(envVars, corev1.EnvVar{
			Name: "DB_PASSWORD",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: instance.Spec.Postgres.PasswordSecret,
					},
					Key: "password",
				},
			},
		})
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		if err := controllerutil.SetControllerReference(instance, deploy, r.Scheme); err != nil {
			return err
		}

		deploy.Spec.Replicas = &replicas
		deploy.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: map[string]string{
				"app":      "odoo",
				"instance": instance.Name,
			},
		}

		podLabels := map[string]string{
			"app":      "odoo",
			"instance": instance.Name,
		}

		deploy.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: podLabels,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "odoo",
						Image: image,
						Ports: []corev1.ContainerPort{
							{ContainerPort: 8069, Name: "odoo"},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "odoo-config",
								MountPath: "/etc/odoo",
								ReadOnly:  true,
							},
							{
								Name:      "addons",
								MountPath: "/mnt/odoo/addons",
								ReadOnly:  true,
							},
						},
						Env:       envVars,
						Resources: instance.Spec.Resources,
					},
				},
				Volumes: []corev1.Volume{
					{
						Name: "odoo-config",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: fmt.Sprintf("%s-config", instance.Name),
								},
							},
						},
					},
					{
						Name: "addons",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: fmt.Sprintf("%s-addons", instance.Name),
								ReadOnly:  true,
							},
						},
					},
				},
			},
		}

		return nil
	})

	return err
}

func (r *OdooInstanceReconciler) reconcileService(ctx context.Context, instance *odoov1.OdooInstance) error {
	svcName := instance.Name
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcName,
			Namespace: instance.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if err := controllerutil.SetControllerReference(instance, svc, r.Scheme); err != nil {
			return err
		}

		svc.Spec = corev1.ServiceSpec{
			Selector: map[string]string{
				"app":      "odoo",
				"instance": instance.Name,
			},
			Ports: []corev1.ServicePort{
				{
					Name:     "odoo",
					Port:     8069,
					Protocol: "TCP",
				},
			},
			Type: corev1.ServiceTypeClusterIP,
		}
		return nil
	})

	return err
}

func (r *OdooInstanceReconciler) handleFinalizer(ctx context.Context, instance *odoov1.OdooInstance) error {
	if controllerutil.ContainsFinalizer(instance, finalizerOdooInstance) {
		controllerutil.RemoveFinalizer(instance, finalizerOdooInstance)
		if err := r.Update(ctx, instance); err != nil {
			return err
		}
	}
	return nil
}

func (r *OdooInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&odoov1.OdooInstance{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.ConfigMap{}).
		Named("odooinstance").
		Complete(r)
}
