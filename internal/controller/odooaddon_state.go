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

// Pure decision logic for the OdooAddon sync state machine. The reconciler gathers
// facts (spec validation, the sync Job, the addon status, the clock) and hands them to
// decideAddonSync, which says what to do next. Nothing here touches the API server, so
// every branch is covered by table-driven tests.

import (
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	addonStaleJobRequeue = 2 * time.Second
	addonCreateRequeue   = 5 * time.Second
	addonAwaitRequeue    = 30 * time.Second
	addonMsgJobConflict  = "job name conflict"
)

// addonSyncAction is the single step the reconciler takes for an addon this pass.
type addonSyncAction int

const (
	// addonActionInvalidSpec: the spec failed validation, so no Job may run.
	addonActionInvalidSpec addonSyncAction = iota
	// addonActionJobConflict: a Job with the sync name exists but this addon does not own it.
	addonActionJobConflict
	// addonActionDeleteStaleJob: the Job was built from a different spec than the current one.
	addonActionDeleteStaleJob
	// addonActionRecordSuccess: the Job succeeded; read its commit and record it.
	addonActionRecordSuccess
	// addonActionRecordFailure: the Job failed; record the reason and its logs.
	addonActionRecordFailure
	// addonActionAwaitJob: the Job is still running; wait for its watch events.
	addonActionAwaitJob
	// addonActionCooldown: the last attempt is too recent to retry yet.
	addonActionCooldown
	// addonActionSkipResync: the addon is pinned to a commit that is already synced.
	addonActionSkipResync
	// addonActionCreateJob: start a new sync Job.
	addonActionCreateJob
)

var addonSyncActionNames = [...]string{
	addonActionInvalidSpec:    "InvalidSpec",
	addonActionJobConflict:    "JobConflict",
	addonActionDeleteStaleJob: "DeleteStaleJob",
	addonActionRecordSuccess:  "RecordSuccess",
	addonActionRecordFailure:  "RecordFailure",
	addonActionAwaitJob:       "AwaitJob",
	addonActionCooldown:       "Cooldown",
	addonActionSkipResync:     "SkipResync",
	addonActionCreateJob:      "CreateJob",
}

func (a addonSyncAction) String() string {
	if int(a) < 0 || int(a) >= len(addonSyncActionNames) {
		return "Unknown"
	}
	return addonSyncActionNames[a]
}

// addonJobObservation is what the reconciler knows about the sync Job.
type addonJobObservation struct {
	Exists            bool
	ControlledByAddon bool
	SpecHash          string
	Succeeded         bool
	Failed            bool
}

// addonSyncInput is every fact decideAddonSync needs.
type addonSyncInput struct {
	// SpecErr is the resolveSyncSpec error text, empty when the spec is valid.
	SpecErr string
	// SpecHash is syncSpecHash of the current spec; unused when SpecErr is set.
	SpecHash string
	Job      addonJobObservation
	// Phase and LastSyncTime come from the addon status.
	Phase        string
	LastSyncTime *time.Time
	// SkipResync is canSkipResync(addon): pinned to a commit that is already synced.
	SkipResync bool
	Now        time.Time
}

// addonSyncDecision is what the reconciler does next.
type addonSyncDecision struct {
	Action addonSyncAction
	// Phase is the phase to write, or "" to leave the phase untouched.
	Phase string
	// Message is the status message for the invalid-spec and conflict actions.
	Message string
	// DeleteJob asks the reconciler to delete the sync Job (background, no waiting).
	DeleteJob bool
	// Requeue is how long to wait before the next reconcile.
	Requeue time.Duration
}

// decideAddonSync maps the current facts onto the next action. Order matters: a Job is
// always judged before any cooldown, and a Job is never judged by a result it did not
// produce for the current spec.
func decideAddonSync(in addonSyncInput) addonSyncDecision {
	if in.SpecErr != "" {
		return addonSyncDecision{
			Action:    addonActionInvalidSpec,
			Phase:     phaseFailed,
			Message:   in.SpecErr,
			DeleteJob: in.Job.Exists && in.Job.ControlledByAddon,
			Requeue:   failureBackoff,
		}
	}

	if in.Job.Exists {
		switch {
		case !in.Job.ControlledByAddon:
			return addonSyncDecision{Action: addonActionJobConflict, Phase: phaseFailed, Message: addonMsgJobConflict, Requeue: failureBackoff}
		case in.Job.SpecHash != in.SpecHash:
			return addonSyncDecision{Action: addonActionDeleteStaleJob, DeleteJob: true, Requeue: addonStaleJobRequeue}
		case in.Job.Succeeded:
			return addonSyncDecision{Action: addonActionRecordSuccess, Phase: phaseSynced, DeleteJob: true, Requeue: resyncInterval}
		case in.Job.Failed:
			return addonSyncDecision{Action: addonActionRecordFailure, Phase: phaseFailed, DeleteJob: true, Requeue: failureBackoff}
		}
		return addonSyncDecision{Action: addonActionAwaitJob, Phase: syncingPhaseFor(in.Phase), Requeue: addonAwaitRequeue}
	}

	if in.LastSyncTime != nil {
		interval := resyncInterval
		if in.Phase == phaseFailed {
			interval = failureBackoff
		}
		if elapsed := in.Now.Sub(*in.LastSyncTime); elapsed < interval {
			return addonSyncDecision{Action: addonActionCooldown, Requeue: interval - elapsed}
		}
	}

	if in.SkipResync {
		return addonSyncDecision{Action: addonActionSkipResync, Requeue: resyncInterval}
	}

	return addonSyncDecision{Action: addonActionCreateJob, Phase: syncingPhaseFor(in.Phase), Requeue: addonCreateRequeue}
}

// syncingPhaseFor returns the phase to show while a sync runs. An addon that is already
// Synced keeps its phase (and Ready=true) during a periodic resync, so OdooInstance keeps
// using its current commit; only an addon that is not Synced shows Syncing.
func syncingPhaseFor(current string) string {
	if current == phaseSynced {
		return ""
	}
	return phaseSyncing
}

// observeSyncJob turns the Job as read from the API into the facts decideAddonSync uses.
// A nil job means the Job does not exist.
func observeSyncJob(job *batchv1.Job, addon *odoov1.OdooAddon) addonJobObservation {
	if job == nil {
		return addonJobObservation{}
	}
	return addonJobObservation{
		Exists:            true,
		ControlledByAddon: metav1.IsControlledBy(job, addon),
		SpecHash:          job.Annotations[annotationSyncSpecHash],
		Succeeded:         job.Status.Succeeded > 0 || upgradeHasCondition(job, batchv1.JobComplete),
		// Only the Failed condition is terminal: status.failed counts pods, and the Job
		// may still be retrying within its backoffLimit.
		Failed: upgradeHasCondition(job, batchv1.JobFailed),
	}
}
