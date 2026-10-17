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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	tiInstanceName = "shop"
	tiInitName     = "shop-init"
	tiNewHash      = "new"
)

func TestInitDecideTable(t *testing.T) {
	name := initUpgradeName(tiInstanceName)
	tests := []struct {
		name string
		in   initInput
		want initDecision
	}{
		{
			name: "not requested: status untouched, nothing pending",
			in:   initInput{requested: false, initialized: false, upgradeName: name},
			want: initDecision{initialized: false},
		},
		{
			name: "not requested but initialized stays initialized",
			in:   initInput{requested: false, initialized: true, upgradeName: name},
			want: initDecision{initialized: true},
		},
		{
			name: "requested and already initialized: no upgrade, no hold",
			in:   initInput{requested: true, initialized: true, upgradeName: name},
			want: initDecision{initialized: true},
		},
		{
			name: "fresh instance: create the upgrade and hold at zero",
			in:   initInput{requested: true, upgradeName: name},
			want: initDecision{pending: true, ensureUpgrade: true, upgradeName: name, phase: phaseInitializing},
		},
		{
			name: "live Deployment without an init upgrade is adopted, not re-initialised",
			in:   initInput{requested: true, upgradeName: name, liveReplicas: 2},
			want: initDecision{initialized: true, adopted: true},
		},
		{
			name: "live pods but the init upgrade already exists: keep waiting for it",
			in: initInput{requested: true, upgradeName: name, upgradeExists: true,
				upgradePhase: odoov1.UpgradePhaseRunning, liveReplicas: 1},
			want: initDecision{pending: true, upgradeName: name, phase: phaseInitializing},
		},
		{
			name: "upgrade pending: still initialising",
			in: initInput{requested: true, upgradeName: name, upgradeExists: true,
				upgradePhase: odoov1.UpgradePhasePending},
			want: initDecision{pending: true, upgradeName: name, phase: phaseInitializing},
		},
		{
			name: "upgrade without a phase yet: still initialising",
			in:   initInput{requested: true, upgradeName: name, upgradeExists: true},
			want: initDecision{pending: true, upgradeName: name, phase: phaseInitializing},
		},
		{
			name: "upgrade succeeded: initialised, name cleared",
			in: initInput{requested: true, upgradeName: name, upgradeExists: true,
				upgradePhase: odoov1.UpgradePhaseSucceeded},
			want: initDecision{initialized: true, succeeded: name},
		},
		{
			name: "upgrade failed: stay at zero with the message",
			in: initInput{requested: true, upgradeName: name, upgradeExists: true,
				upgradePhase: odoov1.UpgradePhaseFailed, upgradeMessage: evMsgBoom},
			want: initDecision{pending: true, upgradeName: name, phase: phaseInitFailed, failMessage: evMsgBoom},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := initDecide(tt.in); got != tt.want {
				t.Errorf("initDecide() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestInitUpgradeName(t *testing.T) {
	if got := initUpgradeName(tiInstanceName); got != tiInstanceName+"-init" {
		t.Errorf("initUpgradeName() = %q", got)
	}
}

func TestInitHoldGateRecordsCurrentRevision(t *testing.T) {
	inst := &odoov1.OdooInstance{ObjectMeta: metav1.ObjectMeta{Name: tiInstanceName}}
	inst.Status.AppliedAddonsHash = "old"
	inst.Status.ActiveUpgrade = "stale"
	got := initHoldGate(inst, tiNewHash)
	if got.templateHash != tiNewHash || got.appliedHash != tiNewHash || got.phase != "" || got.ensureUpgrade {
		t.Errorf("initHoldGate() = %+v", got)
	}
	if inst.Status.AppliedAddonsHash != tiNewHash || inst.Status.ActiveUpgrade != "" {
		t.Errorf("status = %q / %q", inst.Status.AppliedAddonsHash, inst.Status.ActiveUpgrade)
	}
}

func TestInitEvents(t *testing.T) {
	if evs := initEvents(initDecision{pending: true, phase: phaseInitializing}); len(evs) != 0 {
		t.Errorf("pending: events = %+v, want none", evs)
	}
	ok := initEvents(initDecision{initialized: true, succeeded: tiInitName})
	if len(ok) != 1 || ok[0].Reason != eventReasonInitialized || ok[0].RelatedUpgrade != tiInitName {
		t.Errorf("succeeded: events = %+v", ok)
	}
	adopted := initEvents(initDecision{initialized: true, adopted: true})
	if len(adopted) != 1 || adopted[0].Reason != eventReasonInitSkipped {
		t.Errorf("adopted: events = %+v", adopted)
	}
}

func TestInstanceConditionsInitPhases(t *testing.T) {
	running := instanceConditions(instanceCondFacts{Phase: phaseInitializing, InitUpgrade: tiInitName})
	ready := findCond(t, running, odoov1.ConditionTypeReady)
	if ready.Status != condFalse || ready.Reason != odoov1.ReasonInitializing {
		t.Errorf("initializing Ready = %+v", ready)
	}
	if d := findCond(t, running, odoov1.ConditionTypeDegraded); d.Status != condFalse {
		t.Errorf("initializing Degraded = %+v", d)
	}

	failed := instanceConditions(instanceCondFacts{Phase: phaseInitFailed, InitUpgrade: tiInitName, InitMessage: "no module"})
	deg := findCond(t, failed, odoov1.ConditionTypeDegraded)
	if deg.Status != condTrue || deg.Reason != odoov1.ReasonInitializationFailed {
		t.Errorf("failed Degraded = %+v", deg)
	}
	if deg.Message != `OdooUpgrade "shop-init" failed; pods stay at 0 replicas: no module` {
		t.Errorf("failed Degraded message = %q", deg.Message)
	}
	noMsg := instanceConditions(instanceCondFacts{Phase: phaseInitFailed, InitUpgrade: tiInitName})
	if m := findCond(t, noMsg, odoov1.ConditionTypeDegraded).Message; m == "" || m[len(m)-len(initMsgFailedDefault):] != initMsgFailedDefault {
		t.Errorf("default message = %q", m)
	}
}

func TestInstanceEventsInitFailed(t *testing.T) {
	before := odoov1.OdooInstanceStatus{InitUpgrade: tiInitName}
	after := odoov1.OdooInstanceStatus{
		InitUpgrade: tiInitName,
		Conditions: instanceConditions(instanceCondFacts{
			Phase: phaseInitFailed, InitUpgrade: tiInitName, InitMessage: evMsgBoom,
		}),
	}
	evs := instanceEvents(before, after, "")
	var found bool
	for _, e := range evs {
		if e.Reason == eventReasonInitFailed && e.RelatedUpgrade == tiInitName {
			found = true
		}
	}
	if !found {
		t.Errorf("no InitializationFailed event in %+v", evs)
	}
}

// findCond returns the condition of the given type or fails the test.
func findCond(t *testing.T, conds []metav1.Condition, typ string) metav1.Condition {
	t.Helper()
	for _, c := range conds {
		if c.Type == typ {
			return c
		}
	}
	t.Fatalf("condition %s not found in %+v", typ, conds)
	return metav1.Condition{}
}
