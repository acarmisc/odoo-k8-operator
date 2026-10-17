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
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	finalizerOdooInstance = "odoo.operator.io/instance-finalizer"
	defaultOdooImage      = "odoo:19.0"
	phaseCreating         = "Creating"
	phaseRunning          = "Running"
	phaseFailing          = "Failed"
)

var odooLogger = logf.Log.WithName("controller_odooinstance")

type OdooInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Recorder emits Events on status transitions. Nil disables events.
	Recorder events.EventRecorder
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

	if instance.DeletionTimestamp != nil {
		return ctrl.Result{}, r.handleFinalizer(ctx, instance)
	}

	if !controllerutil.ContainsFinalizer(instance, finalizerOdooInstance) {
		controllerutil.AddFinalizer(instance, finalizerOdooInstance)
		if err := r.Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	before := instance.Status.DeepCopy()
	result, err := r.reconcileOdooInstance(ctx, instance)
	if err != nil {
		odooLogger.Error(err, "Failed to reconcile OdooInstance")
		instance.Status.Phase = phaseFailing
		instance.Status.Ready = false
		setConditions(&instance.Status.Conditions, instance.Generation, instanceConditions(instanceCondFacts{
			Phase:      phaseFailing,
			ErrReason:  reconcileErrorReason(err),
			ErrMessage: err.Error(),
		}))
		instance.Status.Message = instanceSummary(instance.Status.Conditions)
		if statusErr := r.Status().Update(ctx, instance); statusErr != nil {
			odooLogger.Error(statusErr, "Failed to update instance status")
		} else {
			emitEvents(r.Recorder, instance, instanceEvents(*before, instance.Status, ""))
		}
		return ctrl.Result{}, err
	}

	return result, nil
}

// reconcileOdooInstance runs one pass. It writes the status once at the end and emits the
// transition events for that write.
func (r *OdooInstanceReconciler) reconcileOdooInstance(ctx context.Context, instance *odoov1.OdooInstance) (ctrl.Result, error) {
	before := instance.Status.DeepCopy()
	instance.Status.Phase = phaseCreating
	instance.Status.ObservedGeneration = instance.Generation

	if err := r.reconcilePVC(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	conf, err := renderOdooConf(&instance.Spec)
	if err != nil {
		return ctrl.Result{}, withReason(odoov1.ReasonInvalidConfig, err)
	}
	if err := r.reconcileConfigMap(ctx, instance, conf); err != nil {
		return ctrl.Result{}, err
	}

	secrets, err := r.referencedSecrets(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	addons, err := r.listReadyAddons(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	gate, err := r.gateAddonsRollout(ctx, instance, addonsHash(addons))
	if err != nil {
		return ctrl.Result{}, err
	}
	hashes := rolloutHashes{
		config:  configHash(conf),
		secrets: secretsHash(secrets),
		addons:  gate.templateHash,
	}

	if err := r.reconcileDeployment(ctx, instance, hashes); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcilePDB(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileService(ctx, instance); err != nil {
		return ctrl.Result{}, err
	}

	// Derive addonPaths from Synced OdooAddon objects; avoids a cross-controller status write race.
	instance.Status.AddonPaths = addonMountPaths(addons)

	deploy := &appsv1.Deployment{}
	desired := desiredReplicas(&instance.Spec)
	deployFound := false
	var readyReplicas int32
	if getErr := r.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, deploy); getErr == nil {
		deployFound = true
		readyReplicas = deploy.Status.ReadyReplicas
		if deploy.Status.ReadyReplicas >= desired {
			instance.Status.Phase = phaseRunning
			instance.Status.Ready = true
		} else {
			instance.Status.Phase = phaseCreating
			instance.Status.Ready = false
		}
	} else {
		instance.Status.Phase = phaseCreating
		instance.Status.Ready = false
	}

	// While an upgrade runs the old pods keep serving, so Ready keeps following the
	// Deployment; only the phase text changes. A failed upgrade is not ready.
	result := ctrl.Result{}
	if gate.phase != "" {
		instance.Status.Phase = gate.phase
	}
	if gate.phase == gatePhaseUpgradeFailed {
		instance.Status.Ready = false
		result.RequeueAfter = gateFailedRequeue
	}

	setConditions(&instance.Status.Conditions, instance.Generation, instanceConditions(instanceCondFacts{
		Phase:           instance.Status.Phase,
		DeploymentFound: deployFound,
		ReadyReplicas:   readyReplicas,
		DesiredReplicas: desired,
		ActiveUpgrade:   instance.Status.ActiveUpgrade,
		UpgradeMessage:  gate.upgradeMessage,
	}))
	instance.Status.Message = instanceSummary(instance.Status.Conditions)

	if err := r.Status().Update(ctx, instance); err != nil {
		return result, err
	}
	emitEvents(r.Recorder, instance, instanceEvents(*before, instance.Status, gate.succeededUpgrade))
	return result, nil
}

// referencedSecrets fetches every Secret the pod consumes. A missing Secret is
// reported by name, because the pod could not start without it either.
func (r *OdooInstanceReconciler) referencedSecrets(ctx context.Context, instance *odoov1.OdooInstance) ([]*corev1.Secret, error) {
	refs := []struct {
		field string
		name  string
	}{
		{"spec.postgres.passwordSecret", instance.Spec.Postgres.PasswordSecret},
		{"spec.adminPasswordSecret", instance.Spec.AdminPasswordSecret},
	}

	secrets := make([]*corev1.Secret, 0, len(refs))
	for _, ref := range refs {
		if ref.name == "" {
			continue
		}
		secret := &corev1.Secret{}
		key := types.NamespacedName{Name: ref.name, Namespace: instance.Namespace}
		if err := r.Get(ctx, key, secret); err != nil {
			if errors.IsNotFound(err) {
				return nil, withReason(odoov1.ReasonMissingSecret,
					fmt.Errorf("secret %q referenced by %s does not exist in namespace %q", ref.name, ref.field, instance.Namespace))
			}
			return nil, fmt.Errorf("get secret %q: %w", ref.name, err)
		}
		secrets = append(secrets, secret)
	}
	return secrets, nil
}

// listReadyAddons returns the ready OdooAddons bound to this instance.
func (r *OdooInstanceReconciler) listReadyAddons(ctx context.Context, instance *odoov1.OdooInstance) ([]odoov1.OdooAddon, error) {
	list := &odoov1.OdooAddonList{}
	if err := r.List(ctx, list, client.InNamespace(instance.Namespace)); err != nil {
		return nil, err
	}
	return filterReadyAddons(list.Items, instance.Name), nil
}

func (r *OdooInstanceReconciler) reconcileConfigMap(ctx context.Context, instance *odoov1.OdooInstance, conf string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName(instance.Name),
			Namespace: instance.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		if err := controllerutil.SetControllerReference(instance, cm, r.Scheme); err != nil {
			return err
		}
		cm.Data = map[string]string{confFileName: conf}
		return nil
	})

	return err
}

func (r *OdooInstanceReconciler) reconcileDeployment(ctx context.Context, instance *odoov1.OdooInstance, hashes rolloutHashes) error {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		if err := controllerutil.SetControllerReference(instance, deploy, r.Scheme); err != nil {
			return err
		}

		replicas := desiredReplicas(&instance.Spec)
		deploy.Labels = podLabels(instance.Name)
		deploy.Spec.Replicas = &replicas
		deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels(instance.Name)}
		deploy.Spec.Strategy = deploymentStrategy(replicas, instance.Spec.Data.AccessMode)
		deploy.Spec.Template = buildPodTemplate(instance, hashes)
		return nil
	})

	return err
}

// reconcilePDB keeps a PodDisruptionBudget only while there is more than one
// replica, so a node drain cannot take every pod down at once.
func (r *OdooInstanceReconciler) reconcilePDB(ctx context.Context, instance *odoov1.OdooInstance) error {
	if desiredReplicas(&instance.Spec) <= 1 {
		return r.deletePDBIfOwned(ctx, instance)
	}

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		if err := controllerutil.SetControllerReference(instance, pdb, r.Scheme); err != nil {
			return err
		}
		maxUnavailable := intstr.FromInt32(1)
		pdb.Spec.MinAvailable = nil
		pdb.Spec.MaxUnavailable = &maxUnavailable
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: podLabels(instance.Name)}
		return nil
	})
	return err
}

func (r *OdooInstanceReconciler) deletePDBIfOwned(ctx context.Context, instance *odoov1.OdooInstance) error {
	pdb := &policyv1.PodDisruptionBudget{}
	key := types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}
	if err := r.Get(ctx, key, pdb); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(pdb, instance) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, pdb))
}

func (r *OdooInstanceReconciler) reconcileService(ctx context.Context, instance *odoov1.OdooInstance) error {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		if err := controllerutil.SetControllerReference(instance, svc, r.Scheme); err != nil {
			return err
		}
		// Mutate only the fields we own so server-allocated fields (ClusterIP, etc.) are left alone.
		svc.Spec.Type = corev1.ServiceTypeClusterIP
		svc.Spec.Selector = selectorLabels(instance.Name)
		svc.Spec.Ports = buildServicePorts()
		return nil
	})

	return err
}

// handleFinalizer only releases the finalizer. Deleting an OdooInstance never
// deletes the data PVC (see dataRetainOnDelete); owned children are cleaned
// up by Kubernetes garbage collection.
func (r *OdooInstanceReconciler) handleFinalizer(ctx context.Context, instance *odoov1.OdooInstance) error {
	if controllerutil.ContainsFinalizer(instance, finalizerOdooInstance) {
		controllerutil.RemoveFinalizer(instance, finalizerOdooInstance)
		if err := r.Update(ctx, instance); err != nil {
			return err
		}
	}
	return nil
}

// instancesUsingSecret maps a Secret change to the instances that reference it,
// so a rotated password rolls the pods without waiting for a resync.
func (r *OdooInstanceReconciler) instancesUsingSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &odoov1.OdooInstanceList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		odooLogger.Error(err, "Failed to list OdooInstances for secret", "secret", obj.GetName())
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		inst := &list.Items[i]
		if inst.Spec.Postgres.PasswordSecret != obj.GetName() && inst.Spec.AdminPasswordSecret != obj.GetName() {
			continue
		}
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: inst.Name, Namespace: inst.Namespace},
		})
	}
	return requests
}

func (r *OdooInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&odoov1.OdooInstance{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Owns(&odoov1.OdooUpgrade{}).
		Watches(
			&odoov1.OdooAddon{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				addon, ok := obj.(*odoov1.OdooAddon)
				if !ok || addon.Spec.InstanceRef.Name == "" {
					return nil
				}
				return []reconcile.Request{{
					NamespacedName: types.NamespacedName{
						Name:      addon.Spec.InstanceRef.Name,
						Namespace: addon.Namespace,
					},
				}}
			}),
		).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.instancesUsingSecret),
		).
		Named("odooinstance").
		Complete(r)
}
