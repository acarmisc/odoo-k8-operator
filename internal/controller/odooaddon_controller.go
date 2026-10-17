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
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
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
	phaseSyncing = "Syncing"

	resyncInterval = 5 * time.Minute
	failureBackoff = 30 * time.Second
	pendingRequeue = 10 * time.Second

	// batchControllerUIDLabel is set by the Job controller on every pod of a Job. Selecting
	// pods by the Job's UID keeps logs from a previous Job with the same name out of reach.
	batchControllerUIDLabel = "batch.kubernetes.io/controller-uid"
	// addonLogTailBytes bounds the sync Job log excerpt put into a failure message.
	addonLogTailBytes = 2000
	addonLogReadLimit = 1 << 20
	addonMsgLogHeader = "\n--- last log lines ---\n"
)

var addonLogger = logf.Log.WithName("controller_odooaddon")

type OdooAddonReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clientset reads sync Job pod logs (client.Client has no log-streaming support).
	Clientset kubernetes.Interface
	// MaxConcurrentReconciles is how many OdooAddons reconcile in parallel; <= 0 means 1.
	// The reconciler keeps no mutable state of its own, so concurrent addons are safe.
	MaxConcurrentReconciles int
}

func (r *OdooAddonReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	addonLogger.Info("Reconciling OdooAddon", "request", req.Name)

	addon := &odoov1.OdooAddon{}
	err := r.Get(ctx, req.NamespacedName, addon)
	if err != nil {
		if apierrors.IsNotFound(err) {
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
		return r.markFailed(ctx, addon, "cross-namespace instanceRef is not supported")
	}

	if instanceName == "" {
		return r.pending(ctx, addon)
	}

	instance := &odoov1.OdooInstance{}
	if err := r.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: instanceNamespace}, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return r.pending(ctx, addon)
		}
		return ctrl.Result{}, err
	}

	pvcName := addonsPVCName(instance.Name)
	pvcReady, err := r.ensureVolumeMounted(ctx, instance, pvcName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !pvcReady {
		return r.pending(ctx, addon)
	}

	return r.syncAddon(ctx, addon, instance, pvcName)
}

// syncAddon runs one step of the sync state machine. It never blocks: it inspects the
// sync Job once, acts on what it sees, and returns. Progress comes back through the
// Owns(&batchv1.Job{}) watch, or through RequeueAfter.
func (r *OdooAddonReconciler) syncAddon(ctx context.Context, addon *odoov1.OdooAddon, instance *odoov1.OdooInstance, pvcName string) (ctrl.Result, error) {
	spec, specErr := resolveSyncSpec(addon)
	in := addonSyncInput{
		Phase:      addon.Status.Phase,
		SkipResync: canSkipResync(addon),
		Now:        time.Now(),
	}
	if addon.Status.LastSyncTime != nil {
		last := addon.Status.LastSyncTime.Time
		in.LastSyncTime = &last
	}
	if specErr != nil {
		in.SpecErr = specErr.Error()
	} else {
		in.SpecHash = syncSpecHash(spec)
	}

	job, err := r.getSyncJob(ctx, addon)
	if err != nil {
		return ctrl.Result{}, err
	}
	in.Job = observeSyncJob(job, addon)

	decision := decideAddonSync(in)
	addonLogger.V(1).Info("OdooAddon sync decision", "addon", addon.Name, "action", decision.Action.String())

	switch decision.Action {
	case addonActionInvalidSpec, addonActionJobConflict:
		if decision.DeleteJob {
			if err := r.deleteSyncJob(ctx, job); err != nil {
				return ctrl.Result{}, err
			}
		}
		return r.markFailed(ctx, addon, decision.Message)

	case addonActionDeleteStaleJob:
		if err := r.deleteSyncJob(ctx, job); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: decision.Requeue}, nil

	case addonActionRecordSuccess:
		return r.recordSyncSuccess(ctx, addon, job)

	case addonActionRecordFailure:
		return r.recordSyncFailure(ctx, addon, job)

	case addonActionAwaitJob, addonActionCooldown, addonActionSkipResync:
		if err := r.setPhase(ctx, addon, decision.Phase); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: decision.Requeue}, nil

	case addonActionCreateJob:
		if err := r.createSyncJob(ctx, addon, instance, pvcName, spec); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.setPhase(ctx, addon, decision.Phase); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: decision.Requeue}, nil
	}
	return ctrl.Result{}, fmt.Errorf("unhandled sync action %s", decision.Action)
}

// recordSyncSuccess stores the commit the Job synced and retires the Job. If the commit
// cannot be read, the addon is marked Failed instead, so the Job is retired either way.
func (r *OdooAddonReconciler) recordSyncSuccess(ctx context.Context, addon *odoov1.OdooAddon, job *batchv1.Job) (ctrl.Result, error) {
	commit, err := r.readJobCommitHash(ctx, job)
	if err != nil {
		return r.finishFailedSyncJob(ctx, addon, job, err.Error())
	}

	now := metav1.Now()
	if err := r.writeAddonStatus(ctx, addon, func(s *odoov1.OdooAddonStatus) {
		s.Phase = phaseSynced
		s.Ready = true
		s.ClonedCommit = commit
		s.Message = ""
		s.LastSyncTime = &now
	}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteSyncJob(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// recordSyncFailure stores the Job's failure reason with a log excerpt, then retires the Job.
func (r *OdooAddonReconciler) recordSyncFailure(ctx context.Context, addon *odoov1.OdooAddon, job *batchv1.Job) (ctrl.Result, error) {
	msg := jobFailureReason(job)
	if tail := r.jobLogTail(ctx, job); tail != "" {
		msg += addonMsgLogHeader + tail
	}
	return r.finishFailedSyncJob(ctx, addon, job, msg)
}

func (r *OdooAddonReconciler) finishFailedSyncJob(ctx context.Context, addon *odoov1.OdooAddon, job *batchv1.Job, msg string) (ctrl.Result, error) {
	now := metav1.Now()
	if err := r.writeAddonStatus(ctx, addon, func(s *odoov1.OdooAddonStatus) {
		s.Phase = phaseFailed
		s.Ready = false
		s.Message = msg
		s.LastSyncTime = &now
	}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteSyncJob(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: failureBackoff}, nil
}

// markFailed records a Failed phase with msg and retries after failureBackoff. LastSyncTime
// is set only if it was empty, so the cooldown gate still measures from the first failure.
func (r *OdooAddonReconciler) markFailed(ctx context.Context, addon *odoov1.OdooAddon, msg string) (ctrl.Result, error) {
	addonLogger.Info("OdooAddon sync failed", "addon", addon.Name, "reason", msg)
	now := metav1.Now()
	if err := r.writeAddonStatus(ctx, addon, func(s *odoov1.OdooAddonStatus) {
		s.Phase = phaseFailed
		s.Ready = false
		s.Message = msg
		if s.LastSyncTime == nil {
			s.LastSyncTime = &now
		}
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: failureBackoff}, nil
}

// pending parks an addon whose instance or addons volume is not ready yet.
func (r *OdooAddonReconciler) pending(ctx context.Context, addon *odoov1.OdooAddon) (ctrl.Result, error) {
	if err := r.writeAddonStatus(ctx, addon, func(s *odoov1.OdooAddonStatus) {
		s.Phase = phasePending
		s.Ready = false
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: pendingRequeue}, nil
}

// setPhase writes a Syncing phase; an empty phase leaves the status alone.
func (r *OdooAddonReconciler) setPhase(ctx context.Context, addon *odoov1.OdooAddon, phase string) error {
	if phase == "" {
		return nil
	}
	return r.writeAddonStatus(ctx, addon, func(s *odoov1.OdooAddonStatus) {
		s.Phase = phase
		s.Message = ""
	})
}

// writeAddonStatus applies mutate to the addon status and writes it only when something
// changed. Writing an unchanged status would still wake this reconciler through the
// OdooAddon watch, so skipping no-op writes keeps the loop quiet.
func (r *OdooAddonReconciler) writeAddonStatus(ctx context.Context, addon *odoov1.OdooAddon, mutate func(*odoov1.OdooAddonStatus)) error {
	before := addon.Status.DeepCopy()
	mutate(&addon.Status)
	if equality.Semantic.DeepEqual(before, &addon.Status) {
		return nil
	}
	return r.Status().Update(ctx, addon)
}

func (r *OdooAddonReconciler) ensureVolumeMounted(ctx context.Context, instance *odoov1.OdooInstance, pvcName string) (bool, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: instance.Namespace}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
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

// createSyncJob creates the sync Job for spec. An existing Job of the same name is not an
// error: the next reconcile sees it and judges it.
func (r *OdooAddonReconciler) createSyncJob(ctx context.Context, addon *odoov1.OdooAddon, instance *odoov1.OdooInstance, pvcName string, spec syncSpec) error {
	nodeName := r.odooPodNode(ctx, instance)
	job := buildSyncJob(addon, spec, syncJobOptions{
		Image:            syncJobImage,
		PVCName:          pvcName,
		NodeName:         nodeName,
		AddonsAccessMode: instance.Spec.Addons.AccessMode,
	})
	if err := controllerutil.SetControllerReference(addon, job, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create sync job: %w", err)
	}
	return nil
}

// getSyncJob returns the addon's sync Job, or nil when it does not exist.
func (r *OdooAddonReconciler) getSyncJob(ctx context.Context, addon *odoov1.OdooAddon) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: syncJobName(addon.Name), Namespace: addon.Namespace}, job)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return job, nil
}

// deleteSyncJob starts a background delete of job and returns without waiting. The UID
// precondition stops it from deleting a newer Job that reused the name. A Job that is
// already gone or was replaced is not an error.
func (r *OdooAddonReconciler) deleteSyncJob(ctx context.Context, job *batchv1.Job) error {
	propagation := metav1.DeletePropagationBackground
	uid := job.UID
	err := r.Delete(ctx, job, &client.DeleteOptions{
		PropagationPolicy: &propagation,
		Preconditions:     &metav1.Preconditions{UID: &uid},
	})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	return err
}

// jobFailureReason describes why a failed Job failed, from its conditions when available.
func jobFailureReason(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return strings.TrimSpace(strings.Join(upgradeNonEmpty(c.Reason, c.Message), ": "))
		}
	}
	return fmt.Sprintf("sync job %s failed", job.Name)
}

// jobPod returns the newest pod of job, selected by the Job's UID.
func (r *OdooAddonReconciler) jobPod(ctx context.Context, job *batchv1.Job) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{
		batchControllerUIDLabel: string(job.UID),
	}); err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no pod found for job %s", job.Name)
	}
	newest := &pods.Items[0]
	for i := range pods.Items {
		if pods.Items[i].CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = &pods.Items[i]
		}
	}
	return newest, nil
}

func (r *OdooAddonReconciler) readJobLogs(ctx context.Context, job *batchv1.Job) (string, error) {
	if r.Clientset == nil {
		return "", errors.New("no clientset configured to read job logs")
	}
	pod, err := r.jobPod(ctx, job)
	if err != nil {
		return "", err
	}
	stream, err := r.Clientset.CoreV1().Pods(job.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = stream.Close() }()
	data, err := io.ReadAll(io.LimitReader(stream, addonLogReadLimit))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// jobLogTail returns the last ~2000 bytes of the Job's logs, best effort.
func (r *OdooAddonReconciler) jobLogTail(ctx context.Context, job *batchv1.Job) string {
	logs, err := r.readJobLogs(ctx, job)
	if err != nil {
		return ""
	}
	return upgradeTail(logs, addonLogTailBytes)
}

// readJobCommitHash returns the commit SHA a sync Job printed: the last line of its logs.
func (r *OdooAddonReconciler) readJobCommitHash(ctx context.Context, job *batchv1.Job) (string, error) {
	logs, err := r.readJobLogs(ctx, job)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if len(lines) == 0 || lines[len(lines)-1] == "" {
		return "", fmt.Errorf("sync job %s produced no commit hash output", job.Name)
	}
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

func (r *OdooAddonReconciler) handleFinalizer(ctx context.Context, addon *odoov1.OdooAddon) error {
	if controllerutil.ContainsFinalizer(addon, finalizerOdooAddon) {
		// Best-effort and not awaited: the Job is owned by the addon, so the garbage
		// collector removes it anyway if this delete fails. Only a Job this addon owns is
		// touched; a foreign Job with the same name is left alone.
		if job, err := r.getSyncJob(ctx, addon); err == nil && job != nil && metav1.IsControlledBy(job, addon) {
			_ = r.deleteSyncJob(ctx, job)
		}
		controllerutil.RemoveFinalizer(addon, finalizerOdooAddon)
		return r.Update(ctx, addon)
	}
	return nil
}

func (r *OdooAddonReconciler) SetupWithManager(mgr ctrl.Manager) error {
	maxConcurrent := r.MaxConcurrentReconciles
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&odoov1.OdooAddon{}).
		Owns(&batchv1.Job{}).
		Named("odooaddon").
		WithOptions(crcontroller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(r)
}
