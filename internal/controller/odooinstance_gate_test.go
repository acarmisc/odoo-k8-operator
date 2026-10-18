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

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	tgHashApplied = "aaaaaaaaaaaaaaaa"
	tgHashNew     = "bbbbbbbbbbbbbbbb"
	tgUpgradeName = "inst-upg-bbbbbbbb"
	tgInstance    = "inst"
)

func TestGateDecide(t *testing.T) {
	tests := []struct {
		name string
		in   gateInput
		want gateDecision
	}{
		{
			name: "no upgrade spec rolls to the current revision",
			in:   gateInput{upgradeSet: false, applied: tgHashApplied, current: tgHashNew},
			want: gateDecision{templateHash: tgHashNew, appliedHash: tgHashNew},
		},
		{
			name: "no upgrade spec clears a stale active upgrade",
			in:   gateInput{upgradeSet: false, applied: tgHashApplied, current: tgHashNew, upgradeName: tgUpgradeName, upgradeExists: true, upgradePhase: odoov1.UpgradePhaseRunning},
			want: gateDecision{templateHash: tgHashNew, appliedHash: tgHashNew},
		},
		{
			name: "no upgrade spec and nothing applied yet",
			in:   gateInput{upgradeSet: false, applied: "", current: tgHashNew},
			want: gateDecision{templateHash: tgHashNew, appliedHash: tgHashNew},
		},
		{
			name: "first reconcile with upgrade spec records the hash without upgrading",
			in:   gateInput{upgradeSet: true, applied: "", current: tgHashNew},
			want: gateDecision{templateHash: tgHashNew, appliedHash: tgHashNew},
		},
		{
			name: "unchanged revision with upgrade spec is a no-op",
			in:   gateInput{upgradeSet: true, applied: tgHashApplied, current: tgHashApplied},
			want: gateDecision{templateHash: tgHashApplied, appliedHash: tgHashApplied},
		},
		{
			name: "unchanged revision clears a leftover active upgrade",
			in:   gateInput{upgradeSet: true, applied: tgHashApplied, current: tgHashApplied, upgradeName: tgUpgradeName, upgradeExists: true, upgradePhase: odoov1.UpgradePhasePending},
			want: gateDecision{templateHash: tgHashApplied, appliedHash: tgHashApplied},
		},
		{
			name: "changed revision with no upgrade object creates it and holds the old hash",
			in:   gateInput{upgradeSet: true, applied: tgHashApplied, current: tgHashNew, upgradeName: tgUpgradeName},
			want: gateDecision{templateHash: tgHashApplied, appliedHash: tgHashApplied, activeUpgrade: tgUpgradeName, phase: gatePhaseUpgrading, ensureUpgrade: true},
		},
		{
			name: "pending upgrade holds the old hash",
			in:   gateInput{upgradeSet: true, applied: tgHashApplied, current: tgHashNew, upgradeName: tgUpgradeName, upgradeExists: true, upgradePhase: odoov1.UpgradePhasePending},
			want: gateDecision{templateHash: tgHashApplied, appliedHash: tgHashApplied, activeUpgrade: tgUpgradeName, phase: gatePhaseUpgrading},
		},
		{
			name: "running upgrade holds the old hash",
			in:   gateInput{upgradeSet: true, applied: tgHashApplied, current: tgHashNew, upgradeName: tgUpgradeName, upgradeExists: true, upgradePhase: odoov1.UpgradePhaseRunning},
			want: gateDecision{templateHash: tgHashApplied, appliedHash: tgHashApplied, activeUpgrade: tgUpgradeName, phase: gatePhaseUpgrading},
		},
		{
			name: "upgrade without a phase yet holds the old hash",
			in:   gateInput{upgradeSet: true, applied: tgHashApplied, current: tgHashNew, upgradeName: tgUpgradeName, upgradeExists: true},
			want: gateDecision{templateHash: tgHashApplied, appliedHash: tgHashApplied, activeUpgrade: tgUpgradeName, phase: gatePhaseUpgrading},
		},
		{
			name: "succeeded upgrade advances applied and rolls the new hash",
			in:   gateInput{upgradeSet: true, applied: tgHashApplied, current: tgHashNew, upgradeName: tgUpgradeName, upgradeExists: true, upgradePhase: odoov1.UpgradePhaseSucceeded},
			want: gateDecision{templateHash: tgHashNew, appliedHash: tgHashNew},
		},
		{
			name: "failed upgrade keeps the old hash and reports UpgradeFailed",
			in:   gateInput{upgradeSet: true, applied: tgHashApplied, current: tgHashNew, upgradeName: tgUpgradeName, upgradeExists: true, upgradePhase: odoov1.UpgradePhaseFailed},
			want: gateDecision{templateHash: tgHashApplied, appliedHash: tgHashApplied, activeUpgrade: tgUpgradeName, phase: gatePhaseUpgradeFailed},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := gateDecide(tc.in)
			if got != tc.want {
				t.Errorf("gateDecide(%+v) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

func TestGateUpgradeName(t *testing.T) {
	tests := []struct {
		name     string
		instance string
		hash     string
		want     string
	}{
		{
			name:     "short instance name",
			instance: tgInstance,
			hash:     tgHashNew,
			want:     tgUpgradeName,
		},
		{
			name:     "long instance name is truncated to 63 characters",
			instance: strings.Repeat("a", 60),
			hash:     tgHashNew,
			want:     strings.Repeat("a", 50) + "-upg-bbbbbbbb",
		},
		{
			name:     "truncation never leaves a trailing dash",
			instance: strings.Repeat("a", 49) + "-x",
			hash:     tgHashNew,
			want:     strings.Repeat("a", 49) + "-upg-bbbbbbbb",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := gateUpgradeName(tc.instance, tc.hash)
			if got != tc.want {
				t.Errorf("gateUpgradeName() = %q, want %q", got, tc.want)
			}
			if len(got) > gateMaxNameLen {
				t.Errorf("gateUpgradeName() length %d exceeds %d", len(got), gateMaxNameLen)
			}
		})
	}
}
