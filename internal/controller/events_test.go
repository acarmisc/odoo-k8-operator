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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	evAddonName = "shop-modules"
	evUpgrade   = "shop-upgrade-1a2b3c4d"
	evHash      = "9f8e7d6c5b4a39281716"
	evMsgBoom   = "boom"
	evNamespace = "odoo"
)

// evInstStatus builds an instance status carrying a Degraded condition with reason and message.
func evInstStatus(degradedReason, msg string) odoov1.OdooInstanceStatus {
	st := odoov1.OdooInstanceStatus{}
	if degradedReason != "" {
		setConditions(&st.Conditions, 1, []metav1.Condition{
			newCondition(odoov1.ConditionTypeDegraded, condTrue, degradedReason, msg),
		})
	}
	return st
}

func evTypes(evs []statusEvent) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.Type+" "+e.Reason)
	}
	return out
}

func checkEvents(t *testing.T, got []statusEvent, want []string) {
	t.Helper()
	gotTypes := evTypes(got)
	if strings.Join(gotTypes, "|") != strings.Join(want, "|") {
		t.Fatalf("events = %v, want %v", gotTypes, want)
	}
}

func TestInstanceEvents(t *testing.T) {
	t.Parallel()
	missing := evInstStatus(odoov1.ReasonMissingSecret, `secret "shop-pg" does not exist`)
	upgradeFailed := evInstStatus(odoov1.ReasonUpgradeFailed, "OdooUpgrade failed")

	tests := []struct {
		name      string
		before    odoov1.OdooInstanceStatus
		after     odoov1.OdooInstanceStatus
		succeeded string
		want      []string
		wantRel   string
	}{
		{
			name:   "no change emits nothing",
			before: missing,
			after:  missing,
		},
		{
			name:  "new error emits ReconcileFailed",
			after: missing,
			want:  []string{"Warning ReconcileFailed"},
		},
		{
			name:    "upgrade failure references the upgrade",
			after:   func() odoov1.OdooInstanceStatus { s := upgradeFailed; s.ActiveUpgrade = evUpgrade; return s }(),
			want:    []string{"Warning UpgradeFailed", "Normal UpgradeStarted"},
			wantRel: evUpgrade,
		},
		{
			name:    "rollout started references the upgrade",
			after:   odoov1.OdooInstanceStatus{ActiveUpgrade: evUpgrade},
			want:    []string{"Normal UpgradeStarted"},
			wantRel: evUpgrade,
		},
		{
			name:      "upgrade succeeded",
			before:    odoov1.OdooInstanceStatus{ActiveUpgrade: evUpgrade},
			after:     odoov1.OdooInstanceStatus{AppliedAddonsHash: evHash},
			succeeded: evUpgrade,
			want:      []string{"Normal UpgradeSucceeded", "Normal RolledOut"},
			wantRel:   evUpgrade,
		},
		{
			name:   "applied hash advance emits RolledOut",
			before: odoov1.OdooInstanceStatus{AppliedAddonsHash: "old"},
			after:  odoov1.OdooInstanceStatus{AppliedAddonsHash: evHash},
			want:   []string{"Normal RolledOut"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := instanceEvents(tt.before, tt.after, tt.succeeded)
			checkEvents(t, got, tt.want)
			if tt.wantRel != "" {
				for _, e := range got {
					if !strings.HasPrefix(e.Reason, "Upgrade") {
						continue
					}
					if e.RelatedUpgrade != tt.wantRel {
						t.Fatalf("event %s related upgrade = %q, want %q", e.Reason, e.RelatedUpgrade, tt.wantRel)
					}
				}
			}
		})
	}
}

func TestAddonEvents(t *testing.T) {
	t.Parallel()
	synced := odoov1.OdooAddonStatus{Phase: phaseSynced, ClonedCommit: cdCommit, Modules: []string{cdModule, "geox_theme"}}
	failedWith := func(reason, msg string) odoov1.OdooAddonStatus {
		st := odoov1.OdooAddonStatus{Phase: phaseFailed, Message: msg}
		setConditions(&st.Conditions, 1, addonConditions(st, reason, msg))
		return st
	}

	tests := []struct {
		name   string
		before odoov1.OdooAddonStatus
		after  odoov1.OdooAddonStatus
		want   []string
	}{
		{
			name:   "Failed to Syncing starts a sync",
			before: failedWith(odoov1.ReasonSyncFailed, evMsgBoom),
			after:  odoov1.OdooAddonStatus{Phase: phaseSyncing},
			want:   []string{"Normal SyncStarted"},
		},
		{
			name:   "Syncing to Synced reports the commit and modules",
			before: odoov1.OdooAddonStatus{Phase: phaseSyncing},
			after:  synced,
			want:   []string{"Normal SyncSucceeded"},
		},
		{
			name:   "periodic resync with the same commit is silent",
			before: synced,
			after:  synced,
		},
		{
			name:   "a moved branch reports the new commit",
			before: synced,
			after:  odoov1.OdooAddonStatus{Phase: phaseSynced, ClonedCommit: "ffff", Modules: []string{cdModule}},
			want:   []string{"Normal SyncSucceeded"},
		},
		{
			name:   "failure emits SyncFailed with the reason",
			before: odoov1.OdooAddonStatus{Phase: phaseSyncing},
			after:  failedWith(odoov1.ReasonSyncFailed, odoov1.ReasonSyncFailed),
			want:   []string{"Warning SyncFailed"},
		},
		{
			name:   "a repeated failure with the same message is silent",
			before: failedWith(odoov1.ReasonSyncFailed, evMsgBoom),
			after:  failedWith(odoov1.ReasonSyncFailed, evMsgBoom),
		},
		{
			name:   "module clash emits ModuleConflict",
			before: odoov1.OdooAddonStatus{Phase: phaseSyncing},
			after:  failedWith(odoov1.ReasonModuleConflict, "module geox is already provided by addon other"),
			want:   []string{"Warning ModuleConflict"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checkEvents(t, addonEvents(tt.before, tt.after), tt.want)
		})
	}
}

func TestUpgradeEvents(t *testing.T) {
	t.Parallel()
	running := odoov1.OdooUpgradeStatus{Phase: odoov1.UpgradePhaseRunning, JobName: "job"}
	tests := []struct {
		name   string
		before odoov1.OdooUpgradeStatus
		after  odoov1.OdooUpgradeStatus
		want   []string
	}{
		{
			name:   "pending to running",
			before: odoov1.OdooUpgradeStatus{Phase: odoov1.UpgradePhasePending},
			after:  running,
			want:   []string{"Normal Started"},
		},
		{
			name:   "running to succeeded",
			before: running,
			after:  odoov1.OdooUpgradeStatus{Phase: odoov1.UpgradePhaseSucceeded},
			want:   []string{"Normal Succeeded"},
		},
		{
			name:   "running to failed is a warning",
			before: running,
			after:  odoov1.OdooUpgradeStatus{Phase: odoov1.UpgradePhaseFailed, Message: evMsgBoom},
			want:   []string{"Warning Failed"},
		},
		{
			name:   "succeeded stays succeeded silently",
			before: odoov1.OdooUpgradeStatus{Phase: odoov1.UpgradePhaseSucceeded},
			after:  odoov1.OdooUpgradeStatus{Phase: odoov1.UpgradePhaseSucceeded},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checkEvents(t, upgradeEvents(tt.before, tt.after), tt.want)
		})
	}
}

func TestClipEventNote(t *testing.T) {
	t.Parallel()
	short := "short note"
	if got := clipEventNote(short); got != short {
		t.Fatalf("short note changed: %q", got)
	}
	long := strings.Repeat("x", eventNoteMax*2)
	got := clipEventNote(long)
	if len(got) > eventNoteMax || !strings.HasSuffix(got, "...") {
		t.Fatalf("long note not clipped: len=%d suffix=%q", len(got), got[len(got)-3:])
	}
}

// TestEmitEvents uses the client-go fake recorder, which writes "<type> <reason> <action> <note>"
// in verbose mode, so the assertions can see the action and the message.
func TestEmitEvents(t *testing.T) {
	t.Parallel()
	t.Run("nil recorder is a no-op", func(t *testing.T) {
		t.Parallel()
		addon := &odoov1.OdooAddon{ObjectMeta: metav1.ObjectMeta{Name: evAddonName, Namespace: evNamespace}}
		emitEvents(nil, addon, []statusEvent{{Type: corev1.EventTypeNormal, Reason: "X", Action: "Y", Message: "z"}})
	})

	t.Run("records each event with its action and message", func(t *testing.T) {
		t.Parallel()
		rec := &events.FakeRecorder{Events: make(chan string, 10), Verbose: true}
		inst := &odoov1.OdooInstance{ObjectMeta: metav1.ObjectMeta{Name: cdInstance, Namespace: evNamespace}}
		emitEvents(rec, inst, []statusEvent{
			{Type: corev1.EventTypeWarning, Reason: eventReasonReconcileFailed, Action: eventActionReconcile, Message: evMsgBoom},
			{Type: corev1.EventTypeNormal, Reason: eventReasonUpgradeStarted, Action: eventActionUpgrade,
				Message: "started", RelatedUpgrade: evUpgrade},
		})
		close(rec.Events)
		var got []string
		for line := range rec.Events {
			got = append(got, line)
		}
		if len(got) != 2 {
			t.Fatalf("got %d events, want 2: %v", len(got), got)
		}
		if !strings.HasPrefix(got[0], "Warning ReconcileFailed Reconcile boom") {
			t.Errorf("first event = %q", got[0])
		}
		if !strings.HasPrefix(got[1], "Normal UpgradeStarted Upgrade started") {
			t.Errorf("second event = %q", got[1])
		}
	})
}
