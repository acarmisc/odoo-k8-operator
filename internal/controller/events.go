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

// Kubernetes Events for status transitions. The transition functions compare the status
// before and after a write, so an event is emitted once per change rather than once per
// reconcile. They are pure; emitEvents is the only part that touches the recorder.

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

// Event reasons. Kept as constants because they are part of what users filter on.
const (
	eventReasonReconcileFailed = "ReconcileFailed"
	eventReasonRolledOut       = "RolledOut"
	eventReasonUpgradeStarted  = "UpgradeStarted"
	eventReasonUpgradeOK       = "UpgradeSucceeded"
	eventReasonUpgradeFailed   = "UpgradeFailed"
	eventReasonSyncStarted     = "SyncStarted"
	eventReasonSyncSucceeded   = "SyncSucceeded"
	eventReasonSyncFailed      = "SyncFailed"
	eventReasonModuleConflict  = "ModuleConflict"
	eventReasonCleanupFailed   = "CleanupFailed"
	eventReasonCleanupTimeout  = "CleanupTimeout"
	eventReasonStarted         = "Started"
	eventReasonSucceeded       = "Succeeded"
	eventReasonFailed          = "Failed"

	eventActionReconcile = "Reconcile"
	eventActionSync      = "Sync"
	eventActionCleanup   = "Cleanup"
	eventActionUpgrade   = "Upgrade"

	// eventNoteMax bounds an event message. Job log tails can be large, and an event is
	// meant to be read at a glance; the full text stays in status.message or the logs.
	eventNoteMax = 1024
)

// statusEvent is one Event to emit for a transition.
type statusEvent struct {
	Type    string
	Reason  string
	Action  string
	Message string
	// RelatedUpgrade names an OdooUpgrade in the same namespace to attach as the related object.
	RelatedUpgrade string
}

// newlyTrue returns the condition of type typ from after when it is True and differs from
// the same condition in before. It returns nil otherwise.
func newlyTrue(before, after []metav1.Condition, typ string) *metav1.Condition {
	a := meta.FindStatusCondition(after, typ)
	if a == nil || a.Status != metav1.ConditionTrue {
		return nil
	}
	if b := meta.FindStatusCondition(before, typ); b != nil &&
		b.Status == metav1.ConditionTrue && b.Reason == a.Reason && b.Message == a.Message {
		return nil
	}
	return a
}

// instanceEvents lists the events for an OdooInstance status change. succeededUpgrade is
// the OdooUpgrade that just succeeded in this pass, or "" when none did.
func instanceEvents(before, after odoov1.OdooInstanceStatus, succeededUpgrade string) []statusEvent {
	var evs []statusEvent
	if c := newlyTrue(before.Conditions, after.Conditions, odoov1.ConditionTypeDegraded); c != nil {
		if c.Reason == odoov1.ReasonUpgradeFailed {
			evs = append(evs, statusEvent{
				Type: corev1.EventTypeWarning, Reason: eventReasonUpgradeFailed, Action: eventActionUpgrade,
				Message: c.Message, RelatedUpgrade: after.ActiveUpgrade,
			})
		} else {
			evs = append(evs, statusEvent{
				Type: corev1.EventTypeWarning, Reason: eventReasonReconcileFailed, Action: eventActionReconcile,
				Message: c.Message,
			})
		}
	}
	if after.ActiveUpgrade != "" && after.ActiveUpgrade != before.ActiveUpgrade {
		evs = append(evs, statusEvent{
			Type: corev1.EventTypeNormal, Reason: eventReasonUpgradeStarted, Action: eventActionUpgrade,
			Message:        fmt.Sprintf("OdooUpgrade %q started; pods stay on the previous addons revision", after.ActiveUpgrade),
			RelatedUpgrade: after.ActiveUpgrade,
		})
	}
	if succeededUpgrade != "" {
		evs = append(evs, statusEvent{
			Type: corev1.EventTypeNormal, Reason: eventReasonUpgradeOK, Action: eventActionUpgrade,
			Message:        fmt.Sprintf("OdooUpgrade %q succeeded; rolling out the new addons revision", succeededUpgrade),
			RelatedUpgrade: succeededUpgrade,
		})
	}
	if after.AppliedAddonsHash != "" && after.AppliedAddonsHash != before.AppliedAddonsHash {
		evs = append(evs, statusEvent{
			Type: corev1.EventTypeNormal, Reason: eventReasonRolledOut, Action: eventActionReconcile,
			Message: fmt.Sprintf("pods rolled out with addons revision %s", shortID(after.AppliedAddonsHash)),
		})
	}
	return evs
}

// addonEvents lists the events for an OdooAddon status change.
func addonEvents(before, after odoov1.OdooAddonStatus) []statusEvent {
	var evs []statusEvent
	if after.Phase == phaseSyncing && before.Phase != phaseSyncing {
		evs = append(evs, statusEvent{
			Type: corev1.EventTypeNormal, Reason: eventReasonSyncStarted, Action: eventActionSync,
			Message: "sync Job started",
		})
	}
	if after.Phase == phaseSynced && (before.Phase != phaseSynced || before.ClonedCommit != after.ClonedCommit) {
		modules := "none reported"
		if len(after.Modules) > 0 {
			modules = strings.Join(after.Modules, ", ")
		}
		evs = append(evs, statusEvent{
			Type: corev1.EventTypeNormal, Reason: eventReasonSyncSucceeded, Action: eventActionSync,
			Message: fmt.Sprintf("synced commit %s; modules: %s", shortID(after.ClonedCommit), modules),
		})
	}
	if c := newlyTrue(before.Conditions, after.Conditions, odoov1.ConditionTypeDegraded); c != nil {
		if c.Reason == odoov1.ReasonModuleConflict {
			evs = append(evs, statusEvent{
				Type: corev1.EventTypeWarning, Reason: eventReasonModuleConflict, Action: eventActionSync,
				Message: c.Message,
			})
		} else {
			evs = append(evs, statusEvent{
				Type: corev1.EventTypeWarning, Reason: eventReasonSyncFailed, Action: eventActionSync,
				Message: fmt.Sprintf("%s: %s", c.Reason, c.Message),
			})
		}
	}
	return evs
}

// upgradeEvents lists the events for an OdooUpgrade status change.
func upgradeEvents(before, after odoov1.OdooUpgradeStatus) []statusEvent {
	var evs []statusEvent
	if after.Phase == odoov1.UpgradePhaseRunning && before.Phase != odoov1.UpgradePhaseRunning {
		evs = append(evs, statusEvent{
			Type: corev1.EventTypeNormal, Reason: eventReasonStarted, Action: eventActionUpgrade,
			Message: fmt.Sprintf("upgrade Job %s started", after.JobName),
		})
	}
	if after.Phase == odoov1.UpgradePhaseSucceeded && before.Phase != odoov1.UpgradePhaseSucceeded {
		evs = append(evs, statusEvent{
			Type: corev1.EventTypeNormal, Reason: eventReasonSucceeded, Action: eventActionUpgrade,
			Message: "upgrade Job completed",
		})
	}
	if after.Phase == odoov1.UpgradePhaseFailed && before.Phase != odoov1.UpgradePhaseFailed {
		msg := after.Message
		if msg == "" {
			msg = "upgrade failed"
		}
		evs = append(evs, statusEvent{
			Type: corev1.EventTypeWarning, Reason: eventReasonFailed, Action: eventActionUpgrade,
			Message: msg,
		})
	}
	return evs
}

// emitEvents records evs against regarding. A nil recorder turns events off, so the
// reconcilers work unchanged when no recorder is configured (unit tests, for example).
func emitEvents(rec events.EventRecorder, regarding client.Object, evs []statusEvent) {
	if rec == nil {
		return
	}
	for _, e := range evs {
		var related runtime.Object
		if e.RelatedUpgrade != "" {
			related = &odoov1.OdooUpgrade{ObjectMeta: metav1.ObjectMeta{
				Name: e.RelatedUpgrade, Namespace: regarding.GetNamespace(),
			}}
		}
		rec.Eventf(regarding, related, e.Type, e.Reason, e.Action, "%s", clipEventNote(e.Message))
	}
}

// clipEventNote truncates an event message to eventNoteMax bytes, keeping valid UTF-8.
func clipEventNote(s string) string {
	if len(s) <= eventNoteMax {
		return s
	}
	return strings.ToValidUTF8(s[:eventNoteMax-3], "") + "..."
}
