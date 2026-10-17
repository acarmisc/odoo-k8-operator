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
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	cdCommit      = "0123456789abcdef0123456789abcdef01234567"
	cdShortCommit = "0123456789ab"
	cdUpgradeName = "shop-upgrade-1a2b3c4d"
	cdJobName     = "shop-upgrade-1a2b3c4d-run"
	cdSecretName  = "shop-pg"
	cdUpgradeMsg  = "Odoo error: module not found"
	cdInstanceMsg = "replicas"
	cdNoErrors    = "no reconcile errors"
	cdNoSyncError = "no sync errors"
	cdBackoffWord = "BackoffLimitExceeded"
	cdRetries     = "retries"
	cdModule      = "geox"
	cdInstance    = "shop"
	cdNotBound    = "not Bound"
	cdQueued      = "queued"
)

// wantCond is an expected condition: type, status, reason, and a substring of the message.
type wantCond struct {
	typ    string
	status metav1.ConditionStatus
	reason string
	msg    string
}

func checkConditions(t *testing.T, got []metav1.Condition, want []wantCond) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d conditions, want %d: %+v", len(got), len(want), got)
	}
	for _, w := range want {
		c := meta.FindStatusCondition(got, w.typ)
		if c == nil {
			t.Errorf("condition %s missing", w.typ)
			continue
		}
		if c.Status != w.status {
			t.Errorf("%s status = %s, want %s", w.typ, c.Status, w.status)
		}
		if c.Reason != w.reason {
			t.Errorf("%s reason = %q, want %q", w.typ, c.Reason, w.reason)
		}
		if !strings.Contains(c.Message, w.msg) {
			t.Errorf("%s message = %q, want it to contain %q", w.typ, c.Message, w.msg)
		}
	}
}

func TestInstanceConditions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		facts instanceCondFacts
		want  []wantCond
	}{
		{
			name:  "running with all replicas ready",
			facts: instanceCondFacts{Phase: phaseRunning, DeploymentFound: true, ReadyReplicas: 3, DesiredReplicas: 3},
			want: []wantCond{
				{odoov1.ConditionTypeReady, condTrue, odoov1.ReasonAllReplicasReady, "3/3 replicas ready"},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonReconciled, "no rollout"},
				{odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, cdNoErrors},
			},
		},
		{
			name:  "creating before the Deployment exists",
			facts: instanceCondFacts{Phase: phaseCreating, DesiredReplicas: 1},
			want: []wantCond{
				{odoov1.ConditionTypeReady, condFalse, odoov1.ReasonDeploymentNotReady, "Deployment not created yet"},
				{odoov1.ConditionTypeProgressing, condTrue, odoov1.ReasonReconciling, "rolling out"},
				{odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, cdNoErrors},
			},
		},
		{
			name: "creating with some pods ready",
			facts: instanceCondFacts{Phase: phaseCreating, DeploymentFound: true,
				ReadyReplicas: 1, DesiredReplicas: 3},
			want: []wantCond{
				{odoov1.ConditionTypeReady, condFalse, odoov1.ReasonDeploymentNotReady, "1/3 replicas ready"},
				{odoov1.ConditionTypeProgressing, condTrue, odoov1.ReasonReconciling, "rolling out"},
				{odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, cdNoErrors},
			},
		},
		{
			name: "upgrading keeps Ready from the Deployment",
			facts: instanceCondFacts{Phase: gatePhaseUpgrading, DeploymentFound: true,
				ReadyReplicas: 1, DesiredReplicas: 1, ActiveUpgrade: cdUpgradeName},
			want: []wantCond{
				{odoov1.ConditionTypeReady, condTrue, odoov1.ReasonAllReplicasReady, "1/1"},
				{odoov1.ConditionTypeProgressing, condTrue, odoov1.ReasonUpgrading, cdUpgradeName},
				{odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, cdNoErrors},
			},
		},
		{
			name: "failed upgrade is not ready and carries the upgrade message",
			facts: instanceCondFacts{Phase: gatePhaseUpgradeFailed, DeploymentFound: true,
				ReadyReplicas: 1, DesiredReplicas: 1, ActiveUpgrade: cdUpgradeName, UpgradeMessage: cdUpgradeMsg},
			want: []wantCond{
				{odoov1.ConditionTypeReady, condFalse, odoov1.ReasonUpgradeFailed, cdUpgradeMsg},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonUpgradeFailed, "held"},
				{odoov1.ConditionTypeDegraded, condTrue, odoov1.ReasonUpgradeFailed, cdUpgradeName},
			},
		},
		{
			name:  "missing Secret is Degraded with its reason and message",
			facts: instanceCondFacts{Phase: phaseFailing, ErrReason: odoov1.ReasonMissingSecret, ErrMessage: "secret \"shop-pg\" does not exist"},
			want: []wantCond{
				{odoov1.ConditionTypeReady, condFalse, odoov1.ReasonMissingSecret, cdSecretName},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonReconcileError, "retried"},
				{odoov1.ConditionTypeDegraded, condTrue, odoov1.ReasonMissingSecret, cdSecretName},
			},
		},
		{
			name:  "error without a reason falls back to ReconcileError",
			facts: instanceCondFacts{Phase: phaseFailing, ErrMessage: evMsgBoom},
			want: []wantCond{
				{odoov1.ConditionTypeReady, condFalse, odoov1.ReasonReconcileError, "boom"},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonReconcileError, "retried"},
				{odoov1.ConditionTypeDegraded, condTrue, odoov1.ReasonReconcileError, "boom"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checkConditions(t, instanceConditions(tt.facts), tt.want)
		})
	}
}

func TestInstanceSummary(t *testing.T) {
	t.Parallel()
	degraded := instanceConditions(instanceCondFacts{Phase: phaseFailing, ErrReason: odoov1.ReasonInvalidConfig, ErrMessage: "bad key"})
	creating := instanceConditions(instanceCondFacts{Phase: phaseCreating, DeploymentFound: true, ReadyReplicas: 0, DesiredReplicas: 1})
	running := instanceConditions(instanceCondFacts{Phase: phaseRunning, DeploymentFound: true, ReadyReplicas: 1, DesiredReplicas: 1})

	tests := []struct {
		name  string
		conds []metav1.Condition
		want  string
	}{
		{"degraded shows the error", degraded, "bad key"},
		{"creating shows the replica count", creating, cdInstanceMsg},
		{"running has no summary", running, ""},
		{"no conditions has no summary", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := instanceSummary(tt.conds)
			if tt.want == "" && got != "" {
				t.Fatalf("summary = %q, want empty", got)
			}
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Fatalf("summary = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

func TestAddonConditions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status odoov1.OdooAddonStatus
		reason string
		msg    string
		want   []wantCond
	}{
		{
			name:   "synced",
			status: odoov1.OdooAddonStatus{Phase: phaseSynced, ClonedCommit: cdCommit, Modules: []string{"a", "b"}},
			want: []wantCond{
				{odoov1.ConditionTypeReady, condTrue, odoov1.ReasonSynced, cdShortCommit + " with 2 module(s)"},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonSynced, "no sync in progress"},
				{odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, cdNoSyncError},
			},
		},
		{
			name:   "syncing",
			status: odoov1.OdooAddonStatus{Phase: phaseSyncing},
			want: []wantCond{
				{odoov1.ConditionTypeReady, condFalse, odoov1.ReasonSyncing, "waiting"},
				{odoov1.ConditionTypeProgressing, condTrue, odoov1.ReasonSyncing, "running"},
				{odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, cdNoSyncError},
			},
		},
		{
			name:   "failed with a reason and message",
			status: odoov1.OdooAddonStatus{Phase: phaseFailed, Message: "stored"},
			reason: odoov1.ReasonInvalidSpec,
			msg:    "gitUrl scheme not allowed",
			want: []wantCond{
				{odoov1.ConditionTypeReady, condFalse, odoov1.ReasonInvalidSpec, "scheme"},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonInvalidSpec, cdRetries},
				{odoov1.ConditionTypeDegraded, condTrue, odoov1.ReasonInvalidSpec, "scheme"},
			},
		},
		{
			name:   "failed without a reason uses SyncFailed and the status message",
			status: odoov1.OdooAddonStatus{Phase: phaseFailed, Message: cdBackoffWord},
			want: []wantCond{
				{odoov1.ConditionTypeReady, condFalse, odoov1.ReasonSyncFailed, cdBackoffWord},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonSyncFailed, cdRetries},
				{odoov1.ConditionTypeDegraded, condTrue, odoov1.ReasonSyncFailed, cdBackoffWord},
			},
		},
		{
			name:   "module conflict",
			status: odoov1.OdooAddonStatus{Phase: phaseFailed},
			reason: odoov1.ReasonModuleConflict,
			msg:    "module geox is already provided by addon other",
			want: []wantCond{
				{odoov1.ConditionTypeReady, condFalse, odoov1.ReasonModuleConflict, cdModule},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonModuleConflict, cdRetries},
				{odoov1.ConditionTypeDegraded, condTrue, odoov1.ReasonModuleConflict, cdModule},
			},
		},
		{
			name:   "pending for a missing instance",
			status: odoov1.OdooAddonStatus{Phase: phasePending},
			reason: odoov1.ReasonInstanceNotFound,
			msg:    `OdooInstance "` + cdInstance + `" not found`,
			want: []wantCond{
				{odoov1.ConditionTypeReady, condFalse, odoov1.ReasonInstanceNotFound, cdInstance},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonPending, cdInstance},
				{odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, cdNoSyncError},
			},
		},
		{
			name:   "pending for an unbound volume",
			status: odoov1.OdooAddonStatus{Phase: phasePending},
			reason: odoov1.ReasonVolumeNotBound,
			msg:    cdNotBound,
			want: []wantCond{
				{odoov1.ConditionTypeReady, condFalse, odoov1.ReasonVolumeNotBound, cdNotBound},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonPending, cdNotBound},
				{odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, cdNoSyncError},
			},
		},
		{
			name: "no phase yet",
			want: []wantCond{
				{odoov1.ConditionTypeReady, condFalse, odoov1.ReasonPending, "not reconciled"},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonPending, "not reconciled"},
				{odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, cdNoSyncError},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checkConditions(t, addonConditions(tt.status, tt.reason, tt.msg), tt.want)
		})
	}
}

func TestUpgradeConditions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status odoov1.OdooUpgradeStatus
		reason string
		want   []wantCond
	}{
		{
			name:   "succeeded",
			status: odoov1.OdooUpgradeStatus{Phase: odoov1.UpgradePhaseSucceeded},
			want: []wantCond{
				{odoov1.ConditionTypeSucceeded, condTrue, odoov1.ReasonComplete, "completed"},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonComplete, "finished"},
			},
		},
		{
			name:   "running names the Job",
			status: odoov1.OdooUpgradeStatus{Phase: odoov1.UpgradePhaseRunning, JobName: cdJobName},
			want: []wantCond{
				{odoov1.ConditionTypeSucceeded, condUnknown, odoov1.ReasonRunning, cdJobName},
				{odoov1.ConditionTypeProgressing, condTrue, odoov1.ReasonRunning, cdJobName},
			},
		},
		{
			name:   "failed Job",
			status: odoov1.OdooUpgradeStatus{Phase: odoov1.UpgradePhaseFailed, Message: cdUpgradeMsg},
			reason: odoov1.ReasonJobFailed,
			want: []wantCond{
				{odoov1.ConditionTypeSucceeded, condFalse, odoov1.ReasonJobFailed, cdUpgradeMsg},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonJobFailed, "stopped"},
			},
		},
		{
			name:   "failed with no database is an invalid spec",
			status: odoov1.OdooUpgradeStatus{Phase: odoov1.UpgradePhaseFailed, Message: upgradeMsgNoDatabase},
			reason: odoov1.ReasonInvalidSpec,
			want: []wantCond{
				{odoov1.ConditionTypeSucceeded, condFalse, odoov1.ReasonInvalidSpec, "no database"},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonInvalidSpec, "stopped"},
			},
		},
		{
			name:   "pending for the instance",
			status: odoov1.OdooUpgradeStatus{Phase: odoov1.UpgradePhasePending, Message: upgradeMsgWaitingInstance},
			reason: odoov1.ReasonInstanceNotFound,
			want: []wantCond{
				{odoov1.ConditionTypeSucceeded, condUnknown, odoov1.ReasonInstanceNotFound, "waiting for OdooInstance"},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonPending, "waiting for OdooInstance"},
			},
		},
		{
			name:   "pending in the queue",
			status: odoov1.OdooUpgradeStatus{Phase: odoov1.UpgradePhasePending, Message: cdQueued},
			want: []wantCond{
				{odoov1.ConditionTypeSucceeded, condUnknown, odoov1.ReasonPending, cdQueued},
				{odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonPending, cdQueued},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			checkConditions(t, upgradeConditions(tt.status, tt.reason), tt.want)
		})
	}
}

func TestReconcileErrorReason(t *testing.T) {
	t.Parallel()
	base := fmt.Errorf("base failure")
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"plain error", base, odoov1.ReasonReconcileError},
		{"missing secret", withReason(odoov1.ReasonMissingSecret, base), odoov1.ReasonMissingSecret},
		{"wrapped reason survives further wrapping", fmt.Errorf("outer: %w", withReason(odoov1.ReasonInvalidConfig, base)), odoov1.ReasonInvalidConfig},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := reconcileErrorReason(tt.err); got != tt.want {
				t.Fatalf("reason = %q, want %q", got, tt.want)
			}
		})
	}
	if withReason(odoov1.ReasonMissingSecret, nil) != nil {
		t.Fatal("withReason(nil) must stay nil")
	}
}

func TestAddonFailureReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		logs string
		want string
	}{
		{"empty log", "", odoov1.ReasonSyncFailed},
		{"git error", "fatal: Authentication failed", odoov1.ReasonSyncFailed},
		{"module clash", "module geox is already provided by addon other\n", odoov1.ReasonModuleConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := addonFailureReason(tt.logs); got != tt.want {
				t.Fatalf("reason = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSetConditionsKeepsTransitionTime checks the no-churn rule: repeating the same
// status keeps LastTransitionTime, and only a status flip moves it.
func TestSetConditionsKeepsTransitionTime(t *testing.T) {
	t.Parallel()
	old := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	conds := []metav1.Condition{{
		Type: odoov1.ConditionTypeReady, Status: condFalse, Reason: odoov1.ReasonSyncing,
		Message: "first", LastTransitionTime: old, ObservedGeneration: 1,
	}}

	setConditions(&conds, 2, []metav1.Condition{newCondition(odoov1.ConditionTypeReady, condFalse, odoov1.ReasonSyncing, "second")})
	c := meta.FindStatusCondition(conds, odoov1.ConditionTypeReady)
	if !c.LastTransitionTime.Equal(&old) {
		t.Fatalf("LastTransitionTime moved on an unchanged status: %v", c.LastTransitionTime)
	}
	if c.Message != "second" || c.ObservedGeneration != 2 {
		t.Fatalf("reason, message or observedGeneration not refreshed: %+v", c)
	}

	setConditions(&conds, 2, []metav1.Condition{newCondition(odoov1.ConditionTypeReady, condTrue, odoov1.ReasonSynced, "up")})
	if c = meta.FindStatusCondition(conds, odoov1.ConditionTypeReady); c.LastTransitionTime.Equal(&old) {
		t.Fatal("LastTransitionTime did not move on a status flip")
	}
}

func TestKeptReasonMessage(t *testing.T) {
	t.Parallel()
	if r, m := keptReasonMessage(nil); r != "" || m != "" {
		t.Fatalf("no conditions: got %q, %q", r, m)
	}
	conds := []metav1.Condition{newCondition(odoov1.ConditionTypeReady, condFalse, odoov1.ReasonModuleConflict, "clash")}
	if r, m := keptReasonMessage(conds); r != odoov1.ReasonModuleConflict || m != "clash" {
		t.Fatalf("got %q, %q", r, m)
	}
}

func TestShortID(t *testing.T) {
	t.Parallel()
	if got := shortID(cdCommit); got != cdShortCommit {
		t.Fatalf("shortID(full) = %q", got)
	}
	if got := shortID("abc"); got != "abc" {
		t.Fatalf("shortID(short) = %q", got)
	}
}
