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
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	backupRequeuePending = 10 * time.Second
	backupRequeueRunning = 5 * time.Second
	// backupRequeueScheduled re-checks a scheduled backup so that its node pin follows the
	// running Odoo pod, which the CronJob template captures when it is written.
	backupRequeueScheduled = 10 * time.Minute
	// backupJobGrace covers the window where a freshly created Job is not yet visible in
	// the cache; a missing Job after that is treated as deleted.
	backupJobGrace = 30 * time.Second

	backupLogTailLines = int64(200)
	backupLogTailBytes = 2000
	backupLogReadLimit = 1 << 20

	backupMsgWaitingInstance = "waiting for OdooInstance"
	backupMsgQueued          = "waiting for backup %q on the same instance to finish"
	backupMsgNoDatabase      = "no database configured: set spec.database or the instance's postgres.database"
	backupMsgJobGone         = "backup Job %s no longer exists"
	backupMsgPodFailed       = "backup pod failed"
	backupMsgLogHeader       = "\n--- last log lines ---\n"
	backupMsgCronRejected    = "CronJob was rejected: %v"
	backupMsgNoPodLogs       = "no backup pod logs"
)

// OdooBackupReconciler runs one-shot database and filestore backups as Jobs, and keeps a
// CronJob in sync for scheduled backups.
type OdooBackupReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clientset reads backup pod logs.
	Clientset kubernetes.Interface
	// Recorder emits Events on status transitions. Nil disables events.
	Recorder events.EventRecorder
}

func (r *OdooBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	bk := &odoov1.OdooBackup{}
	if err := r.Get(ctx, req.NamespacedName, bk); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if bk.Spec.Schedule != "" {
		return r.reconcileScheduled(ctx, bk)
	}
	return r.reconcileOneShot(ctx, bk)
}

// reconcileOneShot runs a one-shot backup: validate, wait for the instance and the queue,
// create the Job, then follow it until it finishes.
func (r *OdooBackupReconciler) reconcileOneShot(ctx context.Context, bk *odoov1.OdooBackup) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(bk)
	switch bk.Status.Phase {
	case odoov1.BackupPhaseSucceeded, odoov1.BackupPhaseFailed:
		return ctrl.Result{}, nil
	case odoov1.BackupPhaseRunning:
		if bk.Status.JobName != "" {
			return r.backupMonitor(ctx, bk)
		}
	}

	if err := backupValidateSpec(bk); err != nil {
		return ctrl.Result{}, r.backupWriteStatus(ctx, key, odoov1.BackupPhaseFailed, odoov1.ReasonInvalidSpec,
			func(s *odoov1.OdooBackupStatus) {
				s.Phase = odoov1.BackupPhaseFailed
				s.Message = err.Error()
			})
	}

	inst := &odoov1.OdooInstance{}
	instKey := types.NamespacedName{Name: bk.Spec.InstanceRef.Name, Namespace: bk.Namespace}
	if err := r.Get(ctx, instKey, inst); err != nil {
		if apierrors.IsNotFound(err) {
			return r.backupSetPending(ctx, key, odoov1.ReasonInstanceNotFound, backupMsgWaitingInstance)
		}
		return ctrl.Result{}, err
	}

	var siblings odoov1.OdooBackupList
	if err := r.List(ctx, &siblings, client.InNamespace(bk.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	if decision := backupDecideStart(bk, siblings.Items); !decision.MayStart {
		return r.backupSetPending(ctx, key, odoov1.ReasonPending, fmt.Sprintf(backupMsgQueued, decision.BlockedBy))
	}
	return r.backupStart(ctx, bk, inst)
}

// backupStart creates the one-shot Job (adopting one that already exists) and marks the
// backup Running.
func (r *OdooBackupReconciler) backupStart(ctx context.Context, bk *odoov1.OdooBackup, inst *odoov1.OdooInstance) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(bk)
	database := backupDatabase(bk, inst)
	if database == "" {
		return ctrl.Result{}, r.backupWriteStatus(ctx, key, odoov1.BackupPhaseFailed, odoov1.ReasonInvalidSpec,
			func(s *odoov1.OdooBackupStatus) {
				s.Phase = odoov1.BackupPhaseFailed
				s.Message = backupMsgNoDatabase
			})
	}

	nodeName := ""
	if backupNeedsNodePin(&inst.Spec, backupIncludeFilestore(bk)) {
		nodeName = r.backupOdooPodNode(ctx, inst)
	}

	job := buildBackupJob(bk, inst, database, nodeName)
	if err := controllerutil.SetControllerReference(bk, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("failed to create backup job: %w", err)
	}

	now := metav1.Now()
	err := r.backupWriteStatus(ctx, key, odoov1.BackupPhaseRunning, odoov1.ReasonRunning, func(s *odoov1.OdooBackupStatus) {
		s.Phase = odoov1.BackupPhaseRunning
		s.JobName = job.Name
		s.Message = ""
		s.CompletionTime = nil
		if s.StartTime == nil {
			s.StartTime = &now
		}
	})
	return ctrl.Result{RequeueAfter: backupRequeueRunning}, err
}

// backupMonitor maps the state of the backup's Job onto the backup status.
func (r *OdooBackupReconciler) backupMonitor(ctx context.Context, bk *odoov1.OdooBackup) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(bk)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: bk.Namespace, Name: bk.Status.JobName}, job)
	if apierrors.IsNotFound(err) {
		if bk.Status.StartTime != nil && time.Since(bk.Status.StartTime.Time) < backupJobGrace {
			return ctrl.Result{RequeueAfter: backupRequeueRunning}, nil
		}
		jobName := bk.Status.JobName
		return ctrl.Result{}, r.backupWriteStatus(ctx, key, odoov1.BackupPhaseFailed, odoov1.ReasonJobFailed,
			func(s *odoov1.OdooBackupStatus) {
				s.Phase = odoov1.BackupPhaseFailed
				s.Message = fmt.Sprintf(backupMsgJobGone, jobName)
				s.CompletionTime = backupNowPtr()
			})
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	if backupJobSucceeded(job) {
		completion := backupJobCompletion(job)
		result := r.backupResultLine(ctx, bk)
		return ctrl.Result{}, r.backupWriteStatus(ctx, key, odoov1.BackupPhaseSucceeded, odoov1.ReasonComplete,
			func(s *odoov1.OdooBackupStatus) {
				s.Phase = odoov1.BackupPhaseSucceeded
				s.Message = ""
				s.CompletionTime = completion
				s.LastBackup = result
			})
	}

	if failed, reason := backupJobFailure(job); failed {
		msg := reason
		if tail := r.backupLogTail(ctx, bk); tail != "" {
			msg += backupMsgLogHeader + tail
		}
		return ctrl.Result{}, r.backupWriteStatus(ctx, key, odoov1.BackupPhaseFailed, odoov1.ReasonJobFailed,
			func(s *odoov1.OdooBackupStatus) {
				s.Phase = odoov1.BackupPhaseFailed
				s.Message = msg
				s.CompletionTime = backupNowPtr()
			})
	}

	return ctrl.Result{RequeueAfter: backupRequeueRunning}, nil
}

// reconcileScheduled keeps the CronJob of a scheduled backup in step with the spec. The
// CronJob is updated when the spec hash changes. A rejected CronJob puts the backup in
// Failed, and the next reconcile retries it.
func (r *OdooBackupReconciler) reconcileScheduled(ctx context.Context, bk *odoov1.OdooBackup) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(bk)
	if err := backupValidateSpec(bk); err != nil {
		return ctrl.Result{}, r.backupWriteStatus(ctx, key, odoov1.BackupPhaseFailed, odoov1.ReasonInvalidSpec,
			func(s *odoov1.OdooBackupStatus) {
				s.Phase = odoov1.BackupPhaseFailed
				s.Message = err.Error()
			})
	}

	inst := &odoov1.OdooInstance{}
	instKey := types.NamespacedName{Name: bk.Spec.InstanceRef.Name, Namespace: bk.Namespace}
	if err := r.Get(ctx, instKey, inst); err != nil {
		if apierrors.IsNotFound(err) {
			return r.backupSetPending(ctx, key, odoov1.ReasonInstanceNotFound, backupMsgWaitingInstance)
		}
		return ctrl.Result{}, err
	}
	database := backupDatabase(bk, inst)
	if database == "" {
		return ctrl.Result{}, r.backupWriteStatus(ctx, key, odoov1.BackupPhaseFailed, odoov1.ReasonInvalidSpec,
			func(s *odoov1.OdooBackupStatus) {
				s.Phase = odoov1.BackupPhaseFailed
				s.Message = backupMsgNoDatabase
			})
	}

	nodeName := ""
	if backupNeedsNodePin(&inst.Spec, backupIncludeFilestore(bk)) {
		nodeName = r.backupOdooPodNode(ctx, inst)
	}
	desired := buildBackupCronJob(bk, inst, database, nodeName)
	if err := controllerutil.SetControllerReference(bk, desired, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	cron, err := r.backupApplyCronJob(ctx, desired)
	if err != nil {
		if apierrors.IsInvalid(err) {
			return ctrl.Result{}, r.backupWriteStatus(ctx, key, odoov1.BackupPhaseFailed, odoov1.ReasonCronJobRejected,
				func(s *odoov1.OdooBackupStatus) {
					s.Phase = odoov1.BackupPhaseFailed
					s.CronJobName = desired.Name
					s.Message = fmt.Sprintf(backupMsgCronRejected, err)
				})
		}
		return ctrl.Result{}, err
	}

	res := ctrl.Result{RequeueAfter: backupRequeueScheduled}
	return res, r.backupWriteStatus(ctx, key, odoov1.BackupPhaseScheduled, odoov1.ReasonScheduled,
		func(s *odoov1.OdooBackupStatus) {
			s.Phase = odoov1.BackupPhaseScheduled
			s.CronJobName = cron.Name
			s.Message = ""
			s.LastScheduleTime = cron.Status.LastScheduleTime
			s.LastSuccessfulTime = cron.Status.LastSuccessfulTime
		})
}

// backupApplyCronJob creates the CronJob, or updates it when its spec hash differs from the
// desired one. It returns the CronJob as it is after the write (or as it was when up to date).
func (r *OdooBackupReconciler) backupApplyCronJob(ctx context.Context, desired *batchv1.CronJob) (*batchv1.CronJob, error) {
	cur := &batchv1.CronJob{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), cur)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return desired, nil
			}
			return nil, err
		}
		return desired, nil
	}
	if err != nil {
		return nil, err
	}
	if cur.Annotations[backupHashAnnotation] == desired.Annotations[backupHashAnnotation] &&
		equality.Semantic.DeepEqual(cur.Labels, desired.Labels) {
		return cur, nil
	}
	cur.Labels = desired.Labels
	cur.Annotations = desired.Annotations
	cur.Spec = desired.Spec
	if err := r.Update(ctx, cur); err != nil {
		return nil, err
	}
	return cur, nil
}

// backupSetPending records that the backup waits for its instance or for an older backup.
func (r *OdooBackupReconciler) backupSetPending(ctx context.Context, key types.NamespacedName, reason, msg string) (ctrl.Result, error) {
	err := r.backupWriteStatus(ctx, key, odoov1.BackupPhasePending, reason, func(s *odoov1.OdooBackupStatus) {
		s.Phase = odoov1.BackupPhasePending
		s.Message = msg
	})
	return ctrl.Result{RequeueAfter: backupRequeuePending}, err
}

// backupWriteStatus applies mutate to the freshest copy of the backup's status, derives the
// conditions from the result (reason explains a Pending or Failed phase), and writes only when
// something changed. It retries on conflicts and tolerates a deleted backup. A one-shot backup
// in a terminal phase is never changed. A write computed from a stale cache never moves
// Running back to Pending. Transition events are emitted after a real write.
func (r *OdooBackupReconciler) backupWriteStatus(ctx context.Context, key types.NamespacedName, target, reason string,
	mutate func(*odoov1.OdooBackupStatus)) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &odoov1.OdooBackup{}
		if err := r.Get(ctx, key, cur); err != nil {
			return err
		}
		if cur.Spec.Schedule == "" && backupPhaseTerminal(cur.Status.Phase) {
			return nil
		}
		if target == odoov1.BackupPhasePending && cur.Status.Phase == odoov1.BackupPhaseRunning {
			return nil
		}
		before := cur.Status.DeepCopy()
		mutate(&cur.Status)
		cur.Status.ObservedGeneration = cur.Generation
		setConditions(&cur.Status.Conditions, cur.Generation, backupConditions(cur.Status, reason))
		if equality.Semantic.DeepEqual(before, &cur.Status) {
			return nil
		}
		after := cur.Status.DeepCopy()
		if err := r.Status().Update(ctx, cur); err != nil {
			return err
		}
		emitEvents(r.Recorder, cur, backupEvents(*before, *after))
		return nil
	})
	return client.IgnoreNotFound(err)
}

// backupPhaseTerminal reports whether a one-shot backup has finished and must not change.
func backupPhaseTerminal(phase string) bool {
	return phase == odoov1.BackupPhaseSucceeded || phase == odoov1.BackupPhaseFailed
}

// backupJobSucceeded reports whether the Job has completed successfully.
func backupJobSucceeded(job *batchv1.Job) bool {
	if job.Status.Succeeded > 0 {
		return true
	}
	return backupHasCondition(job, batchv1.JobComplete)
}

// backupJobFailure reports whether the Job failed and a human-readable reason.
func backupJobFailure(job *batchv1.Job) (bool, string) {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			parts := []string{c.Reason, c.Message}
			var kept []string
			for _, p := range parts {
				if p != "" {
					kept = append(kept, p)
				}
			}
			return true, strings.Join(kept, ": ")
		}
	}
	if job.Status.Failed > 0 {
		return true, backupMsgPodFailed
	}
	return false, ""
}

func backupHasCondition(job *batchv1.Job, condType batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == condType && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// backupJobCompletion returns the Job's completion time, or now if it has none yet.
func backupJobCompletion(job *batchv1.Job) *metav1.Time {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime
	}
	return backupNowPtr()
}

func backupNowPtr() *metav1.Time {
	now := metav1.Now()
	return &now
}

// backupLatestPod returns the newest pod of the backup, or nil. Best effort.
func (r *OdooBackupReconciler) backupLatestPod(ctx context.Context, bk *odoov1.OdooBackup) *corev1.Pod {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(bk.Namespace), client.MatchingLabels{
		backupLabelKey: backupLabelValue(bk.Name),
	}); err != nil || len(pods.Items) == 0 {
		return nil
	}
	pod := &pods.Items[0]
	for i := range pods.Items {
		if pods.Items[i].CreationTimestamp.After(pod.CreationTimestamp.Time) {
			pod = &pods.Items[i]
		}
	}
	return pod
}

// backupPodLogs returns the last lines of one container of the backup's newest pod.
func (r *OdooBackupReconciler) backupPodLogs(ctx context.Context, bk *odoov1.OdooBackup, container string) string {
	if r.Clientset == nil {
		return ""
	}
	pod := r.backupLatestPod(ctx, bk)
	if pod == nil {
		return ""
	}
	tailLines := backupLogTailLines
	stream, err := r.Clientset.CoreV1().Pods(bk.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: container,
		TailLines: &tailLines,
	}).Stream(ctx)
	if err != nil {
		return ""
	}
	defer func() { _ = stream.Close() }()

	data, err := io.ReadAll(io.LimitReader(stream, backupLogReadLimit))
	if err != nil {
		return ""
	}
	return string(data)
}

// backupResultLine returns the file list a successful backup printed last, best effort.
func (r *OdooBackupReconciler) backupResultLine(ctx context.Context, bk *odoov1.OdooBackup) string {
	return backupParseResult(r.backupPodLogs(ctx, bk, backupMainContainer))
}

// backupLogTail returns the last ~2000 bytes of the failed backup's logs. The main container
// is read first. When it never started, the init container (the dump) is read instead.
func (r *OdooBackupReconciler) backupLogTail(ctx context.Context, bk *odoov1.OdooBackup) string {
	logs := r.backupPodLogs(ctx, bk, backupMainContainer)
	if strings.TrimSpace(logs) == "" {
		logs = r.backupPodLogs(ctx, bk, backupInitContainer)
	}
	if strings.TrimSpace(logs) == "" {
		return ""
	}
	return backupTail(logs, backupLogTailBytes)
}

// backupTail returns the last n bytes of s, starting at a line boundary when possible.
func backupTail(s string, n int) string {
	if len(s) <= n {
		return strings.TrimSpace(s)
	}
	cut := s[len(s)-n:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}
	return strings.TrimSpace(strings.ToValidUTF8(cut, ""))
}

// backupOdooPodNode returns the node of the instance's running Odoo pod, best effort.
// Pinning the backup pod there lets an RWO data volume be mounted by both pods.
func (r *OdooBackupReconciler) backupOdooPodNode(ctx context.Context, inst *odoov1.OdooInstance) string {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(inst.Namespace), client.MatchingLabels{
		labelApp:      appOdoo,
		labelInstance: inst.Name,
	}); err != nil {
		return ""
	}
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning && pods.Items[i].Spec.NodeName != "" {
			return pods.Items[i].Spec.NodeName
		}
	}
	return ""
}

func (r *OdooBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&odoov1.OdooBackup{}).
		Owns(&batchv1.Job{}).
		Owns(&batchv1.CronJob{}).
		Named("odoobackup").
		Complete(r)
}
