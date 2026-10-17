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
	"k8s.io/client-go/tools/events"
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
	// addonCleanupRequeue paces the deletion steps while a cleanup Job runs.
	addonCleanupRequeue = 5 * time.Second
	// cleanupTimeout bounds how long deletion waits for the cleanup Job before it gives up
	// and removes the finalizer anyway.
	cleanupTimeout = 3 * time.Minute

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
	// Recorder emits Events on status transitions. Nil disables events.
	Recorder events.EventRecorder
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
		return r.handleFinalizer(ctx, addon)
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
		return r.markFailed(ctx, addon, odoov1.ReasonInvalidSpec, "cross-namespace instanceRef is not supported")
	}

	if instanceName == "" {
		return r.pending(ctx, addon, odoov1.ReasonInstanceNotFound, "spec.instanceRef.name is empty")
	}

	instance := &odoov1.OdooInstance{}
	if err := r.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: instanceNamespace}, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return r.pending(ctx, addon, odoov1.ReasonInstanceNotFound,
				fmt.Sprintf("OdooInstance %q not found in namespace %q", instanceName, instanceNamespace))
		}
		return ctrl.Result{}, err
	}

	pvcName := addonsPVCName(instance.Name)
	pvcReady, err := r.ensureVolumeMounted(ctx, instance, pvcName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !pvcReady {
		return r.pending(ctx, addon, odoov1.ReasonVolumeNotBound,
			fmt.Sprintf("addons PVC %q is not Bound yet", pvcName))
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
			if err := r.deleteJob(ctx, job); err != nil {
				return ctrl.Result{}, err
			}
		}
		reason := odoov1.ReasonInvalidSpec
		if decision.Action == addonActionJobConflict {
			reason = odoov1.ReasonJobConflict
		}
		return r.markFailed(ctx, addon, reason, decision.Message)

	case addonActionDeleteStaleJob:
		if err := r.deleteJob(ctx, job); err != nil {
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
	commit, modules, err := r.readSyncOutput(ctx, job)
	if err != nil {
		return r.finishFailedSyncJob(ctx, addon, job, odoov1.ReasonSyncFailed, err.Error())
	}

	now := metav1.Now()
	if err := r.writeAddonStatus(ctx, addon, "", "", func(s *odoov1.OdooAddonStatus) {
		s.Phase = phaseSynced
		s.Ready = true
		s.ClonedCommit = commit
		s.Modules = modules
		s.Message = ""
		s.LastSyncTime = &now
	}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteJob(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

// recordSyncFailure stores the Job's failure reason with a log excerpt, then retires the Job.
// The log decides the condition reason: a module clash is reported as ModuleConflict.
func (r *OdooAddonReconciler) recordSyncFailure(ctx context.Context, addon *odoov1.OdooAddon, job *batchv1.Job) (ctrl.Result, error) {
	msg := jobFailureReason(job)
	logs, err := r.readJobLogs(ctx, job)
	if err == nil {
		if tail := upgradeTail(logs, addonLogTailBytes); tail != "" {
			msg += addonMsgLogHeader + tail
		}
	}
	return r.finishFailedSyncJob(ctx, addon, job, addonFailureReason(logs), msg)
}

func (r *OdooAddonReconciler) finishFailedSyncJob(ctx context.Context, addon *odoov1.OdooAddon, job *batchv1.Job, reason, msg string) (ctrl.Result, error) {
	now := metav1.Now()
	if err := r.writeAddonStatus(ctx, addon, reason, msg, func(s *odoov1.OdooAddonStatus) {
		s.Phase = phaseFailed
		s.Ready = false
		s.Message = msg
		s.LastSyncTime = &now
	}); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.deleteJob(ctx, job); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: failureBackoff}, nil
}

// markFailed records a Failed phase with reason and msg and retries after failureBackoff.
// LastSyncTime is set only if it was empty, so the cooldown gate still measures from the
// first failure.
func (r *OdooAddonReconciler) markFailed(ctx context.Context, addon *odoov1.OdooAddon, reason, msg string) (ctrl.Result, error) {
	addonLogger.Info("OdooAddon sync failed", "addon", addon.Name, "reason", msg)
	now := metav1.Now()
	if err := r.writeAddonStatus(ctx, addon, reason, msg, func(s *odoov1.OdooAddonStatus) {
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

// pending parks an addon whose instance or addons volume is not ready yet. reason and msg
// only feed the conditions; the status message is left as it was.
func (r *OdooAddonReconciler) pending(ctx context.Context, addon *odoov1.OdooAddon, reason, msg string) (ctrl.Result, error) {
	if err := r.writeAddonStatus(ctx, addon, reason, msg, func(s *odoov1.OdooAddonStatus) {
		s.Phase = phasePending
		s.Ready = false
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: pendingRequeue}, nil
}

// setPhase writes a Syncing phase when phase is set. It always runs the condition update,
// so an addon whose status predates conditions gets them on its next pass; the write is
// skipped when nothing changed.
func (r *OdooAddonReconciler) setPhase(ctx context.Context, addon *odoov1.OdooAddon, phase string) error {
	reason, msg := "", ""
	if phase == "" {
		// Keeping the phase keeps the reason it was given (for example ModuleConflict).
		reason, msg = keptReasonMessage(addon.Status.Conditions)
	}
	return r.writeAddonStatus(ctx, addon, reason, msg, func(s *odoov1.OdooAddonStatus) {
		if phase == "" {
			return
		}
		s.Phase = phase
		s.Message = ""
	})
}

// writeAddonStatus applies mutate to the addon status, derives the conditions from the
// result (reason and condMsg explain a Pending or Failed phase), and writes only when
// something changed. Writing an unchanged status would still wake this reconciler through
// the OdooAddon watch, so skipping no-op writes keeps the loop quiet. After a real write
// the transition events are emitted.
func (r *OdooAddonReconciler) writeAddonStatus(ctx context.Context, addon *odoov1.OdooAddon,
	reason, condMsg string, mutate func(*odoov1.OdooAddonStatus)) error {
	before := addon.Status.DeepCopy()
	mutate(&addon.Status)
	setConditions(&addon.Status.Conditions, addon.Generation, addonConditions(addon.Status, reason, condMsg))
	if equality.Semantic.DeepEqual(before, &addon.Status) {
		return nil
	}
	after := addon.Status.DeepCopy()
	if err := r.Status().Update(ctx, addon); err != nil {
		return err
	}
	emitEvents(r.Recorder, addon, addonEvents(*before, *after))
	return nil
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

// deleteJob starts a background delete of job and returns without waiting. The UID
// precondition stops it from deleting a newer Job that reused the name. A Job that is
// already gone or was replaced is not an error.
func (r *OdooAddonReconciler) deleteJob(ctx context.Context, job *batchv1.Job) error {
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

// readSyncOutput returns the commit SHA and the module list a sync Job printed. A missing
// or malformed modules line yields no modules; only a missing commit is an error.
func (r *OdooAddonReconciler) readSyncOutput(ctx context.Context, job *batchv1.Job) (string, []string, error) {
	logs, err := r.readJobLogs(ctx, job)
	if err != nil {
		return "", nil, err
	}
	commit, modules := parseSyncOutput(logs)
	if commit == "" {
		return "", nil, fmt.Errorf("sync job %s produced no commit hash output", job.Name)
	}
	return commit, modules, nil
}

// handleFinalizer runs the deletion steps for an addon, one non-blocking pass at a time:
//  1. stop the sync Job, so nothing writes to the volume while it is cleaned;
//  2. run the cleanup Job, which removes this addon's links and release store;
//  3. drop the finalizer.
//
// Step 2 is skipped when the instance or its addons PVC is gone, and step 3 runs
// anyway when the cleanup Job fails or runs past cleanupTimeout, so deletion never sticks.
func (r *OdooAddonReconciler) handleFinalizer(ctx context.Context, addon *odoov1.OdooAddon) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(addon, finalizerOdooAddon) {
		return ctrl.Result{}, nil
	}

	syncJob, err := r.getSyncJob(ctx, addon)
	if err != nil {
		return ctrl.Result{}, err
	}
	// Only a Job this addon owns is touched; a foreign Job with the same name is left alone.
	ownedSyncJob := syncJob != nil && metav1.IsControlledBy(syncJob, addon)

	instance, pvcName, present, err := r.addonVolume(ctx, addon)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !present {
		return r.finishDeletion(ctx, addon, syncJob, ownedSyncJob, nil)
	}

	if ownedSyncJob {
		// The sync Job may still be writing to the volume, so it goes first. Deletion is
		// background; the cleanup Job is created on a later pass once the sync Job is gone.
		if err := r.deleteJob(ctx, syncJob); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: addonStaleJobRequeue}, nil
	}

	cleanup, err := r.getCleanupJob(ctx, addon)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cleanup == nil {
		if err := r.createCleanupJob(ctx, addon, instance, pvcName); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: addonCleanupRequeue}, nil
	}
	if !metav1.IsControlledBy(cleanup, addon) {
		addonLogger.Info("cleanup Job name is taken by another object; skipping volume cleanup", "addon", addon.Name)
		return r.finishDeletion(ctx, addon, nil, false, nil)
	}

	observed := observeSyncJob(cleanup, addon)
	switch {
	case observed.Succeeded:
		return r.finishDeletion(ctx, addon, nil, false, cleanup)
	case observed.Failed:
		addonLogger.Info("cleanup Job failed; removing the finalizer anyway", "addon", addon.Name)
		emitEvents(r.Recorder, addon, []statusEvent{{
			Type: corev1.EventTypeWarning, Reason: eventReasonCleanupFailed, Action: eventActionCleanup,
			Message: "cleanup Job failed; the addon's files may remain on the addons volume. The finalizer is removed anyway",
		}})
		return r.finishDeletion(ctx, addon, nil, false, cleanup)
	case time.Since(cleanup.CreationTimestamp.Time) > cleanupTimeout:
		addonLogger.Info("cleanup Job overran; removing the finalizer anyway", "addon", addon.Name)
		emitEvents(r.Recorder, addon, []statusEvent{{
			Type: corev1.EventTypeWarning, Reason: eventReasonCleanupTimeout, Action: eventActionCleanup,
			Message: fmt.Sprintf("cleanup Job still running after %s; the finalizer is removed anyway", cleanupTimeout),
		}})
		return r.finishDeletion(ctx, addon, nil, false, cleanup)
	}
	return ctrl.Result{RequeueAfter: addonCleanupRequeue}, nil
}

// finishDeletion retires the Jobs still around (both are background deletes), then removes
// the finalizer so the addon object goes away.
func (r *OdooAddonReconciler) finishDeletion(ctx context.Context, addon *odoov1.OdooAddon, syncJob *batchv1.Job, ownedSyncJob bool, cleanup *batchv1.Job) (ctrl.Result, error) {
	if ownedSyncJob {
		if err := r.deleteJob(ctx, syncJob); err != nil {
			return ctrl.Result{}, err
		}
	}
	if cleanup != nil {
		if err := r.deleteJob(ctx, cleanup); err != nil {
			return ctrl.Result{}, err
		}
	}
	controllerutil.RemoveFinalizer(addon, finalizerOdooAddon)
	return ctrl.Result{}, r.Update(ctx, addon)
}

// addonVolume returns the instance and the addons PVC name the addon's volume lives on.
// present is false when either no longer exists, which means there is nothing to clean up.
func (r *OdooAddonReconciler) addonVolume(ctx context.Context, addon *odoov1.OdooAddon) (*odoov1.OdooInstance, string, bool, error) {
	ref := addon.Spec.InstanceRef
	if ref.Name == "" || (ref.Namespace != "" && ref.Namespace != addon.Namespace) {
		return nil, "", false, nil
	}
	instance := &odoov1.OdooInstance{}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: addon.Namespace}, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	pvcName := addonsPVCName(instance.Name)
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: addon.Namespace}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "", false, nil
		}
		return nil, "", false, err
	}
	return instance, pvcName, true, nil
}

// getCleanupJob returns the addon's cleanup Job, or nil when it does not exist.
func (r *OdooAddonReconciler) getCleanupJob(ctx context.Context, addon *odoov1.OdooAddon) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: cleanupJobName(addon.Name), Namespace: addon.Namespace}, job)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return job, nil
}

// createCleanupJob creates the addon's cleanup Job on the node-pinned addons volume. An
// existing Job of the same name is not an error: the next pass judges it.
func (r *OdooAddonReconciler) createCleanupJob(ctx context.Context, addon *odoov1.OdooAddon, instance *odoov1.OdooInstance, pvcName string) error {
	job := buildCleanupJob(addon, syncJobOptions{
		Image:            syncJobImage,
		PVCName:          pvcName,
		NodeName:         r.odooPodNode(ctx, instance),
		AddonsAccessMode: instance.Spec.Addons.AccessMode,
	})
	if err := controllerutil.SetControllerReference(addon, job, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create cleanup job: %w", err)
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
