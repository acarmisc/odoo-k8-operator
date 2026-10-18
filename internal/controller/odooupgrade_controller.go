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
	"slices"
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
	upgradeRequeuePending = 10 * time.Second
	upgradeRequeueRunning = 5 * time.Second
	// upgradeJobGrace covers the window where a freshly created Job is not yet visible
	// in the cache; a missing Job after that is treated as deleted.
	upgradeJobGrace = 30 * time.Second

	upgradeLogTailLines = int64(200)
	upgradeLogTailBytes = 2000
	upgradeLogReadLimit = 1 << 20

	upgradeMsgWaitingInstance = "waiting for OdooInstance"
	upgradeMsgQueued          = "waiting for upgrade %q on the same instance to finish"
	upgradeMsgNoDatabase      = "no database configured"
	upgradeMsgJobGone         = "upgrade Job %s no longer exists"
	upgradeMsgPodFailed       = "upgrade pod failed"
	upgradeMsgLogHeader       = "\n--- last log lines ---\n"
)

// OdooUpgradeReconciler runs one-shot `odoo -u/-i ... --stop-after-init` Jobs against an
// OdooInstance's database.
type OdooUpgradeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clientset reads Job pod logs.
	Clientset kubernetes.Interface
	// Recorder emits Events on status transitions. Nil disables events.
	Recorder events.EventRecorder
}

func (r *OdooUpgradeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	up := &odoov1.OdooUpgrade{}
	if err := r.Get(ctx, req.NamespacedName, up); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if up.Status.Phase != "" && len(up.Status.Conditions) == 0 {
		// Upgrades that predate conditions get them once, here.
		if err := r.upgradeBackfillConditions(ctx, req.NamespacedName); err != nil {
			return ctrl.Result{}, err
		}
	}

	switch up.Status.Phase {
	case odoov1.UpgradePhaseSucceeded, odoov1.UpgradePhaseFailed:
		return ctrl.Result{}, nil
	case odoov1.UpgradePhaseRunning:
		if up.Status.JobName != "" {
			return r.upgradeMonitor(ctx, up)
		}
	}

	if err := upgradeValidateSpec(up); err != nil {
		return ctrl.Result{}, r.upgradeWriteStatus(ctx, req.NamespacedName, odoov1.UpgradePhaseFailed, odoov1.ReasonInvalidSpec, func(s *odoov1.OdooUpgradeStatus) {
			s.Phase = odoov1.UpgradePhaseFailed
			s.Message = err.Error()
		})
	}

	inst := &odoov1.OdooInstance{}
	key := types.NamespacedName{Name: up.Spec.InstanceRef.Name, Namespace: up.Namespace}
	if err := r.Get(ctx, key, inst); err != nil {
		if apierrors.IsNotFound(err) {
			return r.upgradeSetPending(ctx, req.NamespacedName, odoov1.ReasonInstanceNotFound, upgradeMsgWaitingInstance)
		}
		return ctrl.Result{}, err
	}

	var siblings odoov1.OdooUpgradeList
	if err := r.List(ctx, &siblings, client.InNamespace(up.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	if decision := upgradeDecideStart(up, siblings.Items); !decision.MayStart {
		return r.upgradeSetPending(ctx, req.NamespacedName, odoov1.ReasonPending,
			fmt.Sprintf(upgradeMsgQueued, decision.BlockedBy))
	}

	return r.upgradeStart(ctx, up, inst)
}

// upgradeStart creates the Job (adopting one that already exists) and marks the upgrade Running.
func (r *OdooUpgradeReconciler) upgradeStart(ctx context.Context, up *odoov1.OdooUpgrade, inst *odoov1.OdooInstance) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(up)
	database := upgradeDatabase(up, inst)
	if database == "" {
		return ctrl.Result{}, r.upgradeWriteStatus(ctx, key, odoov1.UpgradePhaseFailed, odoov1.ReasonInvalidSpec, func(s *odoov1.OdooUpgradeStatus) {
			s.Phase = odoov1.UpgradePhaseFailed
			s.Message = upgradeMsgNoDatabase
		})
	}

	nodeName := ""
	if upgradeNeedsNodePin(&inst.Spec) {
		nodeName = r.upgradeOdooPodNode(ctx, inst)
	}

	job := buildUpgradeJob(up, inst, database, nodeName)
	if err := controllerutil.SetControllerReference(up, job, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("failed to create upgrade job: %w", err)
	}

	now := metav1.Now()
	err := r.upgradeWriteStatus(ctx, key, odoov1.UpgradePhaseRunning, odoov1.ReasonRunning, func(s *odoov1.OdooUpgradeStatus) {
		s.Phase = odoov1.UpgradePhaseRunning
		s.JobName = job.Name
		s.Message = ""
		s.CompletionTime = nil
		if s.StartTime == nil {
			s.StartTime = &now
		}
	})
	return ctrl.Result{RequeueAfter: upgradeRequeueRunning}, err
}

// upgradeMonitor maps the state of the upgrade's Job onto the upgrade status.
func (r *OdooUpgradeReconciler) upgradeMonitor(ctx context.Context, up *odoov1.OdooUpgrade) (ctrl.Result, error) {
	key := client.ObjectKeyFromObject(up)
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: up.Namespace, Name: up.Status.JobName}, job)
	if apierrors.IsNotFound(err) {
		if up.Status.StartTime != nil && time.Since(up.Status.StartTime.Time) < upgradeJobGrace {
			return ctrl.Result{RequeueAfter: upgradeRequeueRunning}, nil
		}
		jobName := up.Status.JobName
		return ctrl.Result{}, r.upgradeWriteStatus(ctx, key, odoov1.UpgradePhaseFailed, odoov1.ReasonJobFailed, func(s *odoov1.OdooUpgradeStatus) {
			s.Phase = odoov1.UpgradePhaseFailed
			s.Message = fmt.Sprintf(upgradeMsgJobGone, jobName)
			s.CompletionTime = upgradeNowPtr()
		})
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	if upgradeJobSucceeded(job) {
		completion := upgradeJobCompletion(job)
		return ctrl.Result{}, r.upgradeWriteStatus(ctx, key, odoov1.UpgradePhaseSucceeded, odoov1.ReasonComplete, func(s *odoov1.OdooUpgradeStatus) {
			s.Phase = odoov1.UpgradePhaseSucceeded
			s.Message = ""
			s.CompletionTime = completion
		})
	}

	if failed, reason := upgradeJobFailure(job); failed {
		msg := reason
		if tail := r.upgradeLogTail(ctx, up); tail != "" {
			msg += upgradeMsgLogHeader + tail
		}
		return ctrl.Result{}, r.upgradeWriteStatus(ctx, key, odoov1.UpgradePhaseFailed, odoov1.ReasonJobFailed, func(s *odoov1.OdooUpgradeStatus) {
			s.Phase = odoov1.UpgradePhaseFailed
			s.Message = msg
			s.CompletionTime = upgradeNowPtr()
		})
	}

	return ctrl.Result{RequeueAfter: upgradeRequeueRunning}, nil
}

// upgradeSetPending records that the upgrade waits for its instance or for an older upgrade.
// reason is InstanceNotFound or Pending and only feeds the Succeeded condition.
func (r *OdooUpgradeReconciler) upgradeSetPending(ctx context.Context, key types.NamespacedName, reason, msg string) (ctrl.Result, error) {
	err := r.upgradeWriteStatus(ctx, key, odoov1.UpgradePhasePending, reason, func(s *odoov1.OdooUpgradeStatus) {
		s.Phase = odoov1.UpgradePhasePending
		s.Message = msg
	})
	return ctrl.Result{RequeueAfter: upgradeRequeuePending}, err
}

// upgradeWriteStatus applies mutate to the freshest copy of the upgrade's status, derives
// the conditions from the result (reason explains a Pending or Failed phase), and writes
// only when something changed. It retries on conflicts and tolerates a deleted upgrade.
// target is the phase being written: a write computed from a stale cache must never leave
// a terminal phase, or move Running back to Pending. Transition events are emitted after
// a real write.
func (r *OdooUpgradeReconciler) upgradeWriteStatus(ctx context.Context, key types.NamespacedName, target, reason string,
	mutate func(*odoov1.OdooUpgradeStatus)) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &odoov1.OdooUpgrade{}
		if err := r.Get(ctx, key, cur); err != nil {
			return err
		}
		if upgradePhaseTerminal(cur.Status.Phase) {
			return nil
		}
		if target == odoov1.UpgradePhasePending && cur.Status.Phase == odoov1.UpgradePhaseRunning {
			return nil
		}
		before := cur.Status.DeepCopy()
		mutate(&cur.Status)
		cur.Status.ObservedGeneration = cur.Generation
		setConditions(&cur.Status.Conditions, cur.Generation, upgradeConditions(cur.Status, reason))
		if equality.Semantic.DeepEqual(before, &cur.Status) {
			return nil
		}
		after := cur.Status.DeepCopy()
		if err := r.Status().Update(ctx, cur); err != nil {
			return err
		}
		emitEvents(r.Recorder, cur, upgradeEvents(*before, *after))
		return nil
	})
	return client.IgnoreNotFound(err)
}

// upgradeBackfillConditions writes the conditions of an upgrade that has a phase but no
// conditions yet. It touches nothing else and does nothing once conditions exist.
func (r *OdooUpgradeReconciler) upgradeBackfillConditions(ctx context.Context, key types.NamespacedName) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &odoov1.OdooUpgrade{}
		if err := r.Get(ctx, key, cur); err != nil {
			return err
		}
		if cur.Status.Phase == "" || len(cur.Status.Conditions) > 0 {
			return nil
		}
		setConditions(&cur.Status.Conditions, cur.Generation, upgradeConditions(cur.Status, ""))
		return r.Status().Update(ctx, cur)
	})
	return client.IgnoreNotFound(err)
}

// upgradePhaseTerminal reports whether an upgrade has finished and must not change again.
func upgradePhaseTerminal(phase string) bool {
	return phase == odoov1.UpgradePhaseSucceeded || phase == odoov1.UpgradePhaseFailed
}

// upgradeValidateSpec rejects specs that can never produce a meaningful Job.
func upgradeValidateSpec(up *odoov1.OdooUpgrade) error {
	if up.Spec.InstanceRef.Name == "" {
		return errors.New("instanceRef.name is required")
	}
	if ns := up.Spec.InstanceRef.Namespace; ns != "" && ns != up.Namespace {
		return fmt.Errorf("instanceRef.namespace %q must be empty or the upgrade's namespace %q", ns, up.Namespace)
	}
	if slices.Contains(up.Spec.Modules, upgradeAllModules) && len(up.Spec.Modules) > 1 {
		return fmt.Errorf("modules: %q must be the only entry when used", upgradeAllModules)
	}
	if up.Spec.AdminUserPasswordSecret != "" && len(up.Spec.InstallModules) == 0 {
		return errors.New("adminUserPasswordSecret requires installModules")
	}
	return nil
}

// upgradeJobSucceeded reports whether the Job has completed successfully.
func upgradeJobSucceeded(job *batchv1.Job) bool {
	if job.Status.Succeeded > 0 {
		return true
	}
	return upgradeHasCondition(job, batchv1.JobComplete)
}

// upgradeJobFailure reports whether the Job failed and a human-readable reason.
func upgradeJobFailure(job *batchv1.Job) (bool, string) {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true, strings.TrimSpace(strings.Join(upgradeNonEmpty(c.Reason, c.Message), ": "))
		}
	}
	if job.Status.Failed > 0 {
		return true, upgradeMsgPodFailed
	}
	return false, ""
}

func upgradeHasCondition(job *batchv1.Job, condType batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == condType && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// upgradeJobCompletion returns the Job's completion time, or now if it has none yet.
func upgradeJobCompletion(job *batchv1.Job) *metav1.Time {
	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime
	}
	return upgradeNowPtr()
}

func upgradeNowPtr() *metav1.Time {
	now := metav1.Now()
	return &now
}

func upgradeNonEmpty(values ...string) []string {
	var out []string
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// upgradeLogTail returns the last ~2000 bytes of the upgrade pod's log, best effort.
func (r *OdooUpgradeReconciler) upgradeLogTail(ctx context.Context, up *odoov1.OdooUpgrade) string {
	if r.Clientset == nil {
		return ""
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(up.Namespace), client.MatchingLabels{
		upgradeLabelKey: upgradeLabelValue(up.Name),
	}); err != nil || len(pods.Items) == 0 {
		return ""
	}
	pod := &pods.Items[0]
	for i := range pods.Items {
		if pods.Items[i].CreationTimestamp.After(pod.CreationTimestamp.Time) {
			pod = &pods.Items[i]
		}
	}

	tailLines := upgradeLogTailLines
	stream, err := r.Clientset.CoreV1().Pods(up.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: upgradeContainerName,
		TailLines: &tailLines,
	}).Stream(ctx)
	if err != nil {
		return ""
	}
	defer func() { _ = stream.Close() }()

	data, err := io.ReadAll(io.LimitReader(stream, upgradeLogReadLimit))
	if err != nil {
		return ""
	}
	return upgradeTail(string(data), upgradeLogTailBytes)
}

// upgradeTail returns the last n bytes of s, starting at a line boundary when possible.
func upgradeTail(s string, n int) string {
	if len(s) <= n {
		return strings.TrimSpace(s)
	}
	cut := s[len(s)-n:]
	if i := strings.IndexByte(cut, '\n'); i >= 0 && i < len(cut)-1 {
		cut = cut[i+1:]
	}
	return strings.TrimSpace(strings.ToValidUTF8(cut, ""))
}

// upgradeOdooPodNode returns the node of the instance's running Odoo pod, best effort.
// Pinning the upgrade pod there lets an RWO volume be mounted by both pods.
func (r *OdooUpgradeReconciler) upgradeOdooPodNode(ctx context.Context, inst *odoov1.OdooInstance) string {
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

func (r *OdooUpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&odoov1.OdooUpgrade{}).
		Owns(&batchv1.Job{}).
		Named("odooupgrade").
		Complete(r)
}
