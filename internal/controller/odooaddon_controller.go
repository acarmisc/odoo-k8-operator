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
	"io"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

// Odoo discovers modules by scanning addons_path directories for immediate
// subfolders containing __manifest__.py - the config renders a single flat
// addons_path (see reconcileConfigMap), so every synced module must appear
// directly under addonsMountPath. Modules are symlinks into a release store
// (see buildSyncScript), which keeps that flat layout while switching atomically.
const (
	finalizerOdooAddon = "odoo.operator.io/addon-finalizer"
	addonsMountPath    = "/mnt/odoo/addons"

	phaseFailed  = "Failed"
	phasePending = "Pending"
	phaseSynced  = "Synced"

	resyncInterval = 5 * time.Minute
	failureBackoff = 30 * time.Second
	jobWaitTimeout = 3 * time.Minute
)

var addonLogger = logf.Log.WithName("controller_odooaddon")

type OdooAddonReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clientset reads sync Job pod logs (client.Client has no log-streaming support).
	Clientset kubernetes.Interface
}

func (r *OdooAddonReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	addonLogger.Info("Reconciling OdooAddon", "request", req.Name)

	addon := &odoov1.OdooAddon{}
	err := r.Get(ctx, req.NamespacedName, addon)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		addonLogger.Error(err, "Failed to get OdooAddon")
		return ctrl.Result{}, err
	}

	if addon.DeletionTimestamp != nil {
		return ctrl.Result{}, r.handleFinalizer(ctx, addon)
	}

	if !controllerutil.ContainsFinalizer(addon, finalizerOdooAddon) {
		controllerutil.AddFinalizer(addon, finalizerOdooAddon)
		if err := r.Update(ctx, addon); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.reconcileOdooAddon(ctx, addon)
}

func (r *OdooAddonReconciler) reconcileOdooAddon(ctx context.Context, addon *odoov1.OdooAddon) (ctrl.Result, error) {
	instanceName := addon.Spec.InstanceRef.Name
	instanceNamespace := addon.Namespace
	if addon.Spec.InstanceRef.Namespace != "" && addon.Spec.InstanceRef.Namespace != addon.Namespace {
		return r.fail(ctx, addon, fmt.Errorf("cross-namespace instanceRef is not supported"))
	}

	if instanceName == "" {
		return r.pending(ctx, addon)
	}

	instance := &odoov1.OdooInstance{}
	if err := r.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: instanceNamespace}, instance); err != nil {
		if errors.IsNotFound(err) {
			return r.pending(ctx, addon)
		}
		return ctrl.Result{}, err
	}

	pvcName := fmt.Sprintf("%s-addons", instance.Name)
	pvcReady, err := r.ensureVolumeMounted(ctx, instance, pvcName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !pvcReady {
		return r.pending(ctx, addon)
	}

	// Cooldown gate covering both success and failure outcomes. Any Status().Update()
	// below self-triggers another Reconcile via the watch on OdooAddon itself - that
	// happens immediately, regardless of what RequeueAfter we return - so without this
	// check a failing (or even succeeding) sync would retry-loop tightly forever,
	// endlessly deleting and recreating the sync Job before it ever finishes.
	if addon.Status.LastSyncTime != nil {
		interval := resyncInterval
		if addon.Status.Phase == phaseFailed {
			interval = failureBackoff
		}
		if elapsed := time.Since(addon.Status.LastSyncTime.Time); elapsed < interval {
			return ctrl.Result{RequeueAfter: interval - elapsed}, nil
		}
	}

	// A commit-pinned addon that is already synced cannot change, so a resync would only
	// recreate the Job for nothing.
	if canSkipResync(addon) {
		return ctrl.Result{RequeueAfter: resyncInterval}, nil
	}

	// Deliberately no intermediate "Cloning" status write here: runSyncJob blocks
	// synchronously for the whole sync, and a status write mid-flight would trigger
	// the self-reconcile described above before LastSyncTime records this attempt.
	nodeName := r.odooPodNode(ctx, instance)

	commitHash, syncErr := r.runSyncJob(ctx, addon, pvcName, nodeName, instance.Spec.Addons.AccessMode)

	now := metav1.Now()
	addon.Status.LastSyncTime = &now

	if syncErr != nil {
		return r.fail(ctx, addon, syncErr)
	}

	addon.Status.ClonedCommit = commitHash
	addon.Status.Phase = phaseSynced
	addon.Status.Ready = true
	if err := r.Status().Update(ctx, addon); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

func (r *OdooAddonReconciler) fail(ctx context.Context, addon *odoov1.OdooAddon, cause error) (ctrl.Result, error) {
	addonLogger.Error(cause, "OdooAddon sync failed", "addon", addon.Name)
	addon.Status.Phase = phaseFailed
	addon.Status.Ready = false
	if addon.Status.LastSyncTime == nil {
		now := metav1.Now()
		addon.Status.LastSyncTime = &now
	}
	if statusErr := r.Status().Update(ctx, addon); statusErr != nil {
		addonLogger.Error(statusErr, "Failed to update addon status")
	}
	// Returning cause here (instead of nil) would make controller-runtime discard
	// RequeueAfter and retry on its own fast exponential backoff instead, defeating
	// the LastSyncTime cooldown gate above - cause is already logged, so return nil.
	return ctrl.Result{RequeueAfter: failureBackoff}, nil
}

func (r *OdooAddonReconciler) pending(ctx context.Context, addon *odoov1.OdooAddon) (ctrl.Result, error) {
	addon.Status.Phase = phasePending
	addon.Status.Ready = false
	if err := r.Status().Update(ctx, addon); err != nil {
		addonLogger.Error(err, "Failed to update addon status")
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *OdooAddonReconciler) ensureVolumeMounted(ctx context.Context, instance *odoov1.OdooInstance, pvcName string) (bool, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: instance.Namespace}, pvc); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return pvc.Status.Phase == corev1.ClaimBound, nil
}

// odooPodNode returns the node the instance's running Odoo pod is on, best-effort.
// The sync Job's pod is pinned to the same node so both it and the Odoo pod can mount
// the (ReadWriteOnce) addons PVC at once - RWO permits multiple pods on one node, but
// not across nodes. If no running pod is found yet, the scheduler picks freely.
func (r *OdooAddonReconciler) odooPodNode(ctx context.Context, instance *odoov1.OdooInstance) string {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(instance.Namespace), client.MatchingLabels{
		labelApp:      appOdoo,
		labelInstance: instance.Name,
	}); err != nil {
		return ""
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.Spec.NodeName != "" {
			return p.Spec.NodeName
		}
	}
	return ""
}

// runSyncJob validates the spec, replaces any previous sync Job and blocks until the new
// one finishes, returning the commit it synced.
func (r *OdooAddonReconciler) runSyncJob(ctx context.Context, addon *odoov1.OdooAddon, pvcName, nodeName string, accessMode corev1.PersistentVolumeAccessMode) (string, error) {
	spec, err := resolveSyncSpec(addon)
	if err != nil {
		return "", err
	}

	jobName := syncJobName(addon.Name)
	if err := r.deleteJobIfExists(ctx, addon.Namespace, jobName); err != nil {
		return "", err
	}

	job := buildSyncJob(addon, spec, syncJobOptions{
		Image:            syncJobImage,
		PVCName:          pvcName,
		NodeName:         nodeName,
		AddonsAccessMode: accessMode,
	})
	if err := controllerutil.SetControllerReference(addon, job, r.Scheme); err != nil {
		return "", err
	}

	if err := r.Create(ctx, job); err != nil {
		return "", fmt.Errorf("failed to create sync job: %w", err)
	}

	return r.waitForJob(ctx, addon.Namespace, jobName)
}

func (r *OdooAddonReconciler) deleteJobIfExists(ctx context.Context, namespace, jobName string) error {
	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: namespace}, existing)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	propagation := metav1.DeletePropagationForeground
	if err := r.Delete(ctx, existing, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !errors.IsNotFound(err) {
		return err
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: namespace}, &batchv1.Job{})
		if errors.IsNotFound(err) {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("timed out waiting for previous sync job %s to be deleted", jobName)
}

func (r *OdooAddonReconciler) waitForJob(ctx context.Context, namespace, jobName string) (string, error) {
	deadline := time.Now().Add(jobWaitTimeout)
	for time.Now().Before(deadline) {
		job := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: namespace}, job); err != nil {
			// The Job we just created may not be visible yet through the client's cache
			// (informer sync lag) - treat NotFound as "not ready", not a hard failure.
			if errors.IsNotFound(err) {
				time.Sleep(time.Second)
				continue
			}
			return "", err
		}
		if job.Status.Succeeded > 0 {
			return r.readJobCommitHash(ctx, namespace, jobName)
		}
		if job.Status.Failed > 0 {
			logs, logErr := r.readJobLogs(ctx, namespace, jobName)
			if logErr != nil {
				return "", fmt.Errorf("sync job %s failed (could not read logs: %v)", jobName, logErr)
			}
			return "", fmt.Errorf("sync job %s failed: %s", jobName, strings.TrimSpace(logs))
		}
		time.Sleep(3 * time.Second)
	}
	return "", fmt.Errorf("sync job %s did not complete within %s", jobName, jobWaitTimeout)
}

func (r *OdooAddonReconciler) jobPodName(ctx context.Context, namespace, jobName string) (string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"job-name": jobName}); err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pod found for job %s", jobName)
	}
	return pods.Items[0].Name, nil
}

func (r *OdooAddonReconciler) readJobLogs(ctx context.Context, namespace, jobName string) (string, error) {
	podName, err := r.jobPodName(ctx, namespace, jobName)
	if err != nil {
		return "", err
	}
	stream, err := r.Clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()
	data, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (r *OdooAddonReconciler) readJobCommitHash(ctx context.Context, namespace, jobName string) (string, error) {
	logs, err := r.readJobLogs(ctx, namespace, jobName)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if len(lines) == 0 || lines[len(lines)-1] == "" {
		return "", fmt.Errorf("sync job %s produced no commit hash output", jobName)
	}
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

func (r *OdooAddonReconciler) handleFinalizer(ctx context.Context, addon *odoov1.OdooAddon) error {
	if controllerutil.ContainsFinalizer(addon, finalizerOdooAddon) {
		// Best-effort: the Job is owned by the addon, so GC removes it anyway if this fails.
		_ = r.deleteJobIfExists(ctx, addon.Namespace, syncJobName(addon.Name))
		controllerutil.RemoveFinalizer(addon, finalizerOdooAddon)
		return r.Update(ctx, addon)
	}
	return nil
}

func (r *OdooAddonReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&odoov1.OdooAddon{}).
		Owns(&batchv1.Job{}).
		Named("odooaddon").
		Complete(r)
}
