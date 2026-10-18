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

// Pure mapping from reconciler facts to status conditions. Nothing here touches the API
// server. The reconcilers call these after they have decided the phase, and write the
// result with setConditions, which keeps LastTransitionTime stable while a status is
// unchanged.

import (
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	condTrue    = metav1.ConditionTrue
	condFalse   = metav1.ConditionFalse
	condUnknown = metav1.ConditionUnknown

	shortIDLen = 12
)

// newCondition builds a condition. The timestamp is left to meta.SetStatusCondition.
func newCondition(condType string, status metav1.ConditionStatus, reason, message string) metav1.Condition {
	return metav1.Condition{Type: condType, Status: status, Reason: reason, Message: message}
}

// setConditions merges want into conds, stamping each with gen as its observedGeneration.
// meta.SetStatusCondition updates reason and message in place and moves
// LastTransitionTime only when a status flips, so repeating the same facts is a no-op.
func setConditions(conds *[]metav1.Condition, gen int64, want []metav1.Condition) {
	for _, c := range want {
		c.ObservedGeneration = gen
		meta.SetStatusCondition(conds, c)
	}
}

// reasonError tags an error with the condition reason it should produce.
type reasonError struct {
	reason string
	err    error
}

func (e *reasonError) Error() string { return e.err.Error() }

func (e *reasonError) Unwrap() error { return e.err }

// withReason tags err with reason. A nil error stays nil.
func withReason(reason string, err error) error {
	if err == nil {
		return nil
	}
	return &reasonError{reason: reason, err: err}
}

// reconcileErrorReason returns the condition reason for a reconcile error, or
// ReasonReconcileError when the error carries no reason.
func reconcileErrorReason(err error) string {
	var re *reasonError
	if errors.As(err, &re) {
		return re.reason
	}
	return odoov1.ReasonReconcileError
}

// shortID returns the first 12 characters of a commit id or hash, or the whole value if shorter.
func shortID(id string) string {
	if len(id) > shortIDLen {
		return id[:shortIDLen]
	}
	return id
}

// instanceCondFacts is what the OdooInstance conditions are derived from.
type instanceCondFacts struct {
	// Phase is the phase the reconcile settled on (Creating, Running, Upgrading,
	// UpgradeFailed or Failed).
	Phase           string
	DeploymentFound bool
	ReadyReplicas   int32
	DesiredReplicas int32
	// ActiveUpgrade names the OdooUpgrade holding the rollout, if any.
	ActiveUpgrade string
	// UpgradeMessage is the failed upgrade's status.message.
	UpgradeMessage string
	// InitUpgrade names the OdooUpgrade doing the database initialisation, if any.
	InitUpgrade string
	// InitMessage is the failed initialisation's status.message.
	InitMessage string
	// ErrReason and ErrMessage are set when the reconcile returned an error.
	ErrReason  string
	ErrMessage string
}

// instanceConditions maps the instance facts to Ready, Progressing and Degraded.
func instanceConditions(f instanceCondFacts) []metav1.Condition {
	if f.ErrMessage != "" {
		reason := f.ErrReason
		if reason == "" {
			reason = odoov1.ReasonReconcileError
		}
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeReady, condFalse, reason, f.ErrMessage),
			newCondition(odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonReconcileError,
				"reconcile stopped by an error; it is retried"),
			newCondition(odoov1.ConditionTypeDegraded, condTrue, reason, f.ErrMessage),
		}
	}

	replicas := fmt.Sprintf("%d/%d replicas ready", f.ReadyReplicas, f.DesiredReplicas)
	allReady := f.DeploymentFound && f.ReadyReplicas >= f.DesiredReplicas
	readyCond := func() metav1.Condition {
		if allReady {
			return newCondition(odoov1.ConditionTypeReady, condTrue, odoov1.ReasonAllReplicasReady, replicas)
		}
		msg := "waiting for pods: " + replicas
		if !f.DeploymentFound {
			msg = "Deployment not created yet"
		}
		return newCondition(odoov1.ConditionTypeReady, condFalse, odoov1.ReasonDeploymentNotReady, msg)
	}
	healthy := newCondition(odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, "no reconcile errors")

	switch f.Phase {
	case phaseRunning:
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeReady, condTrue, odoov1.ReasonAllReplicasReady, replicas),
			newCondition(odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonReconciled, "no rollout in progress"),
			healthy,
		}
	case gatePhaseUpgrading:
		return []metav1.Condition{
			readyCond(),
			newCondition(odoov1.ConditionTypeProgressing, condTrue, odoov1.ReasonUpgrading,
				fmt.Sprintf("OdooUpgrade %q is running; pods stay on the previous addons revision", f.ActiveUpgrade)),
			healthy,
		}
	case phaseInitializing:
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeReady, condFalse, odoov1.ReasonInitializing,
				fmt.Sprintf("OdooUpgrade %q is initialising the database; pods stay at 0 replicas", f.InitUpgrade)),
			newCondition(odoov1.ConditionTypeProgressing, condTrue, odoov1.ReasonInitializing,
				"database initialisation is running"),
			healthy,
		}
	case phaseInitFailed:
		detail := f.InitMessage
		if detail == "" {
			detail = initMsgFailedDefault
		}
		msg := fmt.Sprintf("OdooUpgrade %q failed; pods stay at 0 replicas: %s", f.InitUpgrade, detail)
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeReady, condFalse, odoov1.ReasonInitializationFailed, msg),
			newCondition(odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonInitializationFailed,
				"initialisation stopped; delete the failed OdooUpgrade to retry"),
			newCondition(odoov1.ConditionTypeDegraded, condTrue, odoov1.ReasonInitializationFailed, msg),
		}
	case gatePhaseUpgradeFailed:
		msg := fmt.Sprintf("OdooUpgrade %q failed; pods stay on the previous addons revision", f.ActiveUpgrade)
		if f.UpgradeMessage != "" {
			msg += ": " + f.UpgradeMessage
		}
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeReady, condFalse, odoov1.ReasonUpgradeFailed, msg),
			newCondition(odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonUpgradeFailed,
				"rollout held until the upgrade is fixed"),
			newCondition(odoov1.ConditionTypeDegraded, condTrue, odoov1.ReasonUpgradeFailed, msg),
		}
	default:
		// Creating: the Deployment is missing or has fewer ready pods than replicas.
		return []metav1.Condition{
			readyCond(),
			newCondition(odoov1.ConditionTypeProgressing, condTrue, odoov1.ReasonReconciling, "rolling out the Deployment"),
			healthy,
		}
	}
}

// cronCondition maps the cron Deployment to CronReady. It is True once the cron pod is ready,
// False with CronNotReady while a cron pod exists but is not ready, and False with CronPending
// before any cron pod exists. CronReady does not affect the instance Ready condition.
func cronCondition(cron cronOutcome) metav1.Condition {
	switch {
	case !cron.deployFound:
		return newCondition(odoov1.ConditionTypeCronReady, condFalse, odoov1.ReasonCronPending,
			"cron Deployment not created yet")
	case cron.replicas == 0 && cron.held:
		return newCondition(odoov1.ConditionTypeCronReady, condFalse, odoov1.ReasonCronPending,
			"no cron pod yet; pods stay at 0 replicas while the database is initialised")
	case cron.replicas == 0:
		return newCondition(odoov1.ConditionTypeCronReady, condFalse, odoov1.ReasonCronPending,
			"no cron pod yet; with ReadWriteOnce volumes it starts once a web pod runs on the same node")
	case cron.ready < 1:
		return newCondition(odoov1.ConditionTypeCronReady, condFalse, odoov1.ReasonCronNotReady,
			fmt.Sprintf("cron pod not ready: %d/%d replicas ready", cron.ready, cron.replicas))
	default:
		return newCondition(odoov1.ConditionTypeCronReady, condTrue, odoov1.ReasonCronRunning,
			"cron pod ready")
	}
}

// instanceSummary is status.message: the Degraded message while degraded, otherwise the
// Ready message while not ready, otherwise empty.
func instanceSummary(conds []metav1.Condition) string {
	if c := meta.FindStatusCondition(conds, odoov1.ConditionTypeDegraded); c != nil && c.Status == condTrue {
		return c.Message
	}
	if c := meta.FindStatusCondition(conds, odoov1.ConditionTypeReady); c != nil && c.Status != condTrue {
		return c.Message
	}
	return ""
}

// keptReasonMessage returns the reason and message of the Ready condition. A write that
// leaves the phase unchanged passes them back in, so the conditions keep the reason the
// phase was first given.
func keptReasonMessage(conds []metav1.Condition) (string, string) {
	c := meta.FindStatusCondition(conds, odoov1.ConditionTypeReady)
	if c == nil {
		return "", ""
	}
	return c.Reason, c.Message
}

// addonModuleConflictMarker is the text the sync script prints when a module is already
// provided by another addon. It must match buildSyncScript.
const addonModuleConflictMarker = "is already provided by addon"

// addonFailureReason returns the condition reason for a failed sync Job from its log.
func addonFailureReason(logs string) string {
	if strings.Contains(logs, addonModuleConflictMarker) {
		return odoov1.ReasonModuleConflict
	}
	return odoov1.ReasonSyncFailed
}

// addonConditions maps the addon status to Ready, Progressing and Degraded. reason and
// msg explain a Pending or Failed phase; when empty, defaults are used. Failed reads the
// message from st when msg is empty.
func addonConditions(st odoov1.OdooAddonStatus, reason, msg string) []metav1.Condition {
	switch st.Phase {
	case phaseSynced:
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeReady, condTrue, odoov1.ReasonSynced,
				fmt.Sprintf("synced commit %s with %d module(s)", shortID(st.ClonedCommit), len(st.Modules))),
			newCondition(odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonSynced, "no sync in progress"),
			newCondition(odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, "no sync errors"),
		}
	case phaseSyncing:
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeReady, condFalse, odoov1.ReasonSyncing, "waiting for the sync Job"),
			newCondition(odoov1.ConditionTypeProgressing, condTrue, odoov1.ReasonSyncing, "sync Job is running"),
			newCondition(odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, "no sync errors"),
		}
	case phaseFailed:
		if reason == "" {
			reason = odoov1.ReasonSyncFailed
		}
		if msg == "" {
			msg = st.Message
		}
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeReady, condFalse, reason, msg),
			newCondition(odoov1.ConditionTypeProgressing, condFalse, reason, "not syncing; the operator retries"),
			newCondition(odoov1.ConditionTypeDegraded, condTrue, reason, msg),
		}
	case phasePending:
		if reason == "" {
			reason = odoov1.ReasonPending
		}
		if msg == "" {
			msg = "waiting for the instance"
		}
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeReady, condFalse, reason, msg),
			newCondition(odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonPending, msg),
			newCondition(odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, "no sync errors"),
		}
	default:
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeReady, condFalse, odoov1.ReasonPending, "not reconciled yet"),
			newCondition(odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonPending, "not reconciled yet"),
			newCondition(odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, "no sync errors"),
		}
	}
}

// upgradeConditions maps the upgrade status to Succeeded and Progressing. reason explains a
// Pending or Failed phase (for example InstanceNotFound or InvalidSpec); when empty, a
// default is used. Messages come from st.Message and st.JobName.
func upgradeConditions(st odoov1.OdooUpgradeStatus, reason string) []metav1.Condition {
	switch st.Phase {
	case odoov1.UpgradePhaseSucceeded:
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeSucceeded, condTrue, odoov1.ReasonComplete, "upgrade Job completed"),
			newCondition(odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonComplete, "upgrade finished"),
		}
	case odoov1.UpgradePhaseRunning:
		msg := fmt.Sprintf("upgrade Job %s is running", st.JobName)
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeSucceeded, condUnknown, odoov1.ReasonRunning, msg),
			newCondition(odoov1.ConditionTypeProgressing, condTrue, odoov1.ReasonRunning, msg),
		}
	case odoov1.UpgradePhaseFailed:
		if reason == "" {
			reason = odoov1.ReasonJobFailed
		}
		msg := st.Message
		if msg == "" {
			msg = "upgrade failed"
		}
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeSucceeded, condFalse, reason, msg),
			newCondition(odoov1.ConditionTypeProgressing, condFalse, reason, "upgrade stopped"),
		}
	default:
		if reason == "" {
			reason = odoov1.ReasonPending
		}
		msg := st.Message
		if msg == "" {
			msg = "waiting to start"
		}
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeSucceeded, condUnknown, reason, msg),
			newCondition(odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonPending, msg),
		}
	}
}
