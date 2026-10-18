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

// Dedicated cron Deployment (spec.cron). With cron enabled the web pods run with
// max_cron_threads = 0 and "<name>-cron" runs the scheduled actions: one replica, no HTTP,
// no Service. Its pod template shares the web template's hashes, so both roll together and
// respect the upgrade gate.

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	suffixCron = "-cron"
	// appOdooCron is the app label of the cron pods. It must differ from appOdoo so the web
	// Service and the web PodDisruptionBudget never select a cron pod.
	appOdooCron    = "odoo-cron"
	labelComponent = "app.kubernetes.io/component"
	componentCron  = "cron"

	odooArgNoHTTP         = "--no-http"
	odooArgMaxCronThreads = "--max-cron-threads"
	defaultMaxCronThreads = int32(2)

	eventActionCron         = "Cron"
	eventReasonCronEnabled  = "CronEnabled"
	eventReasonCronDisabled = "CronDisabled"
)

// cronOutcome is what one reconcile pass found and did for the cron Deployment.
type cronOutcome struct {
	enabled     bool
	deployFound bool
	// held is true while the instance is initialising, which keeps the cron pod at zero.
	held bool
	// replicas and ready are status.replicas and status.readyReplicas of the cron Deployment.
	replicas int32
	ready    int32
	// events are the Events for changes made in this pass (created or removed).
	events []statusEvent
}

func cronEnabled(spec *odoov1.OdooInstanceSpec) bool {
	return spec.Cron != nil && spec.Cron.Enabled
}

func cronDeploymentName(instanceName string) string {
	return instanceName + suffixCron
}

// cronSelectorLabels are immutable once the cron Deployment exists. They do not match the web
// Service or PodDisruptionBudget, which select app=odoo.
func cronSelectorLabels(instanceName string) map[string]string {
	return map[string]string{
		labelApp:      appOdooCron,
		labelInstance: instanceName,
	}
}

// cronPodLabels are the cron selector labels plus the standard Kubernetes labels.
func cronPodLabels(instanceName string) map[string]string {
	labels := cronSelectorLabels(instanceName)
	labels[labelAppName] = appOdoo
	labels[labelAppInstance] = instanceName
	labels[labelManagedBy] = managedByOperator
	labels[labelComponent] = componentCron
	return labels
}

// cronMaxThreads is the --max-cron-threads of the cron pod.
func cronMaxThreads(spec *odoov1.OdooInstanceSpec) int32 {
	if spec.Cron == nil || spec.Cron.MaxCronThreads == nil {
		return defaultMaxCronThreads
	}
	return *spec.Cron.MaxCronThreads
}

// cronResources are spec.cron.resources when set, otherwise the web resources or the defaults.
func cronResources(spec *odoov1.OdooInstanceSpec) corev1.ResourceRequirements {
	if spec.Cron != nil && spec.Cron.Resources != nil &&
		(spec.Cron.Resources.Requests != nil || spec.Cron.Resources.Limits != nil) {
		return *spec.Cron.Resources
	}
	return odooResources(spec)
}

// cronReplicas is the pod count of the cron Deployment: one, or zero while initialising.
func cronReplicas(held bool) int32 {
	if held {
		return 0
	}
	return 1
}

// buildCronArgs are the web args plus --no-http and the cron worker count.
func buildCronArgs(instance *odoov1.OdooInstance) []string {
	args := buildOdooArgs(instance)
	return append(args,
		odooArgNoHTTP,
		fmt.Sprintf("%s=%d", odooArgMaxCronThreads, cronMaxThreads(&instance.Spec)),
	)
}

// buildCronPodTemplate renders the cron pod template. It has no ports and no probes. Its
// annotations are the web pod's rollout hashes, so both templates change together.
func buildCronPodTemplate(instance *odoov1.OdooInstance, hashes rolloutHashes) corev1.PodTemplateSpec {
	container := corev1.Container{
		Name:      appOdoo,
		Args:      buildCronArgs(instance),
		Resources: cronResources(&instance.Spec),
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      cronPodLabels(instance.Name),
			Annotations: podAnnotations(hashes),
		},
		Spec: odooPodSpec(instance, container, colocationAffinity(instance.Name, &instance.Spec)),
	}
}

// reconcileCronDeployment creates or updates "<name>-cron" with the given replica count.
func (r *OdooInstanceReconciler) reconcileCronDeployment(ctx context.Context, instance *odoov1.OdooInstance,
	hashes rolloutHashes, replicas int32,
) (*appsv1.Deployment, controllerutil.OperationResult, error) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cronDeploymentName(instance.Name),
			Namespace: instance.Namespace,
		},
	}
	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		if err := controllerutil.SetControllerReference(instance, deploy, r.Scheme); err != nil {
			return err
		}
		deploy.Labels = cronPodLabels(instance.Name)
		deploy.Spec.Replicas = int32Ptr(replicas)
		deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: cronSelectorLabels(instance.Name)}
		// Recreate: a single cron worker must never run twice, and an RWO volume cannot be
		// attached to a surge pod on another node.
		deploy.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		deploy.Spec.Template = buildCronPodTemplate(instance, hashes)
		return nil
	})
	return deploy, op, err
}

// deleteCronDeployment removes "<name>-cron" if the instance controls it. It reports whether it
// deleted anything.
func (r *OdooInstanceReconciler) deleteCronDeployment(ctx context.Context, instance *odoov1.OdooInstance) (bool, error) {
	deploy := &appsv1.Deployment{}
	key := types.NamespacedName{Name: cronDeploymentName(instance.Name), Namespace: instance.Namespace}
	if err := r.Get(ctx, key, deploy); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(deploy, instance) {
		return false, nil
	}
	if err := client.IgnoreNotFound(r.Delete(ctx, deploy)); err != nil {
		return false, err
	}
	return true, nil
}

// reconcileCron makes the cron Deployment match spec.cron. held keeps the cron pod at zero
// replicas while the database is being initialised.
func (r *OdooInstanceReconciler) reconcileCron(ctx context.Context, instance *odoov1.OdooInstance,
	hashes rolloutHashes, held bool,
) (cronOutcome, error) {
	if !cronEnabled(&instance.Spec) {
		deleted, err := r.deleteCronDeployment(ctx, instance)
		if err != nil {
			return cronOutcome{}, err
		}
		out := cronOutcome{}
		if deleted {
			out.events = []statusEvent{{
				Type: corev1.EventTypeNormal, Reason: eventReasonCronDisabled, Action: eventActionCron,
				Message: fmt.Sprintf("Deployment %q deleted; the web pods run the scheduled actions again",
					cronDeploymentName(instance.Name)),
			}}
		}
		return out, nil
	}

	deploy, op, err := r.reconcileCronDeployment(ctx, instance, hashes, cronReplicas(held))
	if err != nil {
		return cronOutcome{}, err
	}
	out := cronOutcome{
		enabled:     true,
		deployFound: true,
		held:        held,
		replicas:    deploy.Status.Replicas,
		ready:       deploy.Status.ReadyReplicas,
	}
	if op == controllerutil.OperationResultCreated {
		out.events = []statusEvent{{
			Type: corev1.EventTypeNormal, Reason: eventReasonCronEnabled, Action: eventActionCron,
			Message: fmt.Sprintf("Deployment %q created with %d cron worker(s); the web pods run no cron workers",
				cronDeploymentName(instance.Name), cronMaxThreads(&instance.Spec)),
		}}
	}
	return out, nil
}

// applyCronCondition sets CronReady while cron is enabled and removes it when it is disabled.
func applyCronCondition(conds *[]metav1.Condition, gen int64, cron cronOutcome) {
	if !cron.enabled {
		meta.RemoveStatusCondition(conds, odoov1.ConditionTypeCronReady)
		return
	}
	setConditions(conds, gen, []metav1.Condition{cronCondition(cron)})
}
