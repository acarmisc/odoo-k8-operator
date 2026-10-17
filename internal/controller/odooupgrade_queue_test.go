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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	tuOtherInstance = "staging"
	tuNameA         = "a"
	tuNameB         = "b"
	tuNameC         = "c"
	tuNameD         = "d"
)

var tuBaseTime = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// tuQueued builds an upgrade for instance with the given phase and age (seconds after tuBaseTime).
func tuQueued(name, instance, phase string, ageSeconds int) odoov1.OdooUpgrade {
	return odoov1.OdooUpgrade{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         tuNamespace,
			CreationTimestamp: metav1.NewTime(tuBaseTime.Add(time.Duration(ageSeconds) * time.Second)),
		},
		Spec: odoov1.OdooUpgradeSpec{InstanceRef: odoov1.OdooInstanceRef{Name: instance}},
		Status: odoov1.OdooUpgradeStatus{
			Phase: phase,
		},
	}
}

func TestUpgradeDecideStart(t *testing.T) {
	const (
		running = odoov1.UpgradePhaseRunning
		pending = odoov1.UpgradePhasePending
		done    = odoov1.UpgradePhaseSucceeded
		failed  = odoov1.UpgradePhaseFailed
	)
	tests := []struct {
		name      string
		candidate odoov1.OdooUpgrade
		all       []odoov1.OdooUpgrade
		mayStart  bool
		blockedBy string
	}{
		{
			name:      "alone may start",
			candidate: tuQueued(tuNameA, tuInstanceName, pending, 0),
			all:       []odoov1.OdooUpgrade{tuQueued(tuNameA, tuInstanceName, pending, 0)},
			mayStart:  true,
		},
		{
			name:      "fresh object without status may start when alone",
			candidate: tuQueued(tuNameA, tuInstanceName, "", 0),
			all:       []odoov1.OdooUpgrade{tuQueued(tuNameA, tuInstanceName, "", 0)},
			mayStart:  true,
		},
		{
			name:      "running sibling blocks",
			candidate: tuQueued(tuNameB, tuInstanceName, pending, 10),
			all: []odoov1.OdooUpgrade{
				tuQueued(tuNameA, tuInstanceName, running, 0),
				tuQueued(tuNameB, tuInstanceName, pending, 10),
			},
			blockedBy: tuNameA,
		},
		{
			name:      "running sibling blocks even when newer than candidate",
			candidate: tuQueued(tuNameA, tuInstanceName, pending, 0),
			all: []odoov1.OdooUpgrade{
				tuQueued(tuNameA, tuInstanceName, pending, 0),
				tuQueued(tuNameB, tuInstanceName, running, 10),
			},
			blockedBy: tuNameB,
		},
		{
			name:      "older pending sibling blocks",
			candidate: tuQueued(tuNameB, tuInstanceName, pending, 20),
			all: []odoov1.OdooUpgrade{
				tuQueued(tuNameA, tuInstanceName, pending, 10),
				tuQueued(tuNameB, tuInstanceName, pending, 20),
			},
			blockedBy: tuNameA,
		},
		{
			name:      "oldest pending may start even with newer pending siblings",
			candidate: tuQueued(tuNameA, tuInstanceName, pending, 10),
			all: []odoov1.OdooUpgrade{
				tuQueued(tuNameA, tuInstanceName, pending, 10),
				tuQueued(tuNameB, tuInstanceName, pending, 20),
				tuQueued(tuNameC, tuInstanceName, "", 30),
			},
			mayStart: true,
		},
		{
			name:      "same creation time breaks ties by name",
			candidate: tuQueued(tuNameB, tuInstanceName, pending, 5),
			all: []odoov1.OdooUpgrade{
				tuQueued(tuNameB, tuInstanceName, pending, 5),
				tuQueued(tuNameA, tuInstanceName, pending, 5),
			},
			blockedBy: tuNameA,
		},
		{
			name:      "terminal siblings never block",
			candidate: tuQueued(tuNameD, tuInstanceName, pending, 40),
			all: []odoov1.OdooUpgrade{
				tuQueued(tuNameA, tuInstanceName, done, 0),
				tuQueued(tuNameB, tuInstanceName, failed, 10),
				tuQueued(tuNameD, tuInstanceName, pending, 40),
			},
			mayStart: true,
		},
		{
			name:      "other instances never block",
			candidate: tuQueued(tuNameB, tuInstanceName, pending, 20),
			all: []odoov1.OdooUpgrade{
				tuQueued(tuNameA, tuOtherInstance, running, 0),
				tuQueued(tuNameB, tuInstanceName, pending, 20),
			},
			mayStart: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upgradeDecideStart(&tt.candidate, tt.all)
			if got.MayStart != tt.mayStart {
				t.Errorf("MayStart = %v, want %v (decision %+v)", got.MayStart, tt.mayStart, got)
			}
			if got.BlockedBy != tt.blockedBy {
				t.Errorf("BlockedBy = %q, want %q", got.BlockedBy, tt.blockedBy)
			}
		})
	}
}
