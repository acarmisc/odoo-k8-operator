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
	bqInstA     = "inst-a"
	bqInstB     = "inst-b"
	bqFirst     = "bk-first"
	bqSecond    = "bk-second"
	bqCron      = "bk-cron"
	bqSchedule  = "0 3 * * *"
	bqTieFirst  = "bk-tie-a"
	bqTieSecond = "bk-tie-b"
)

var bqT0 = time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)

func bqBackup(name, instance, phase string, created time.Time) odoov1.OdooBackup {
	return odoov1.OdooBackup{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(created)},
		Spec:       odoov1.OdooBackupSpec{InstanceRef: odoov1.OdooInstanceRef{Name: instance}},
		Status:     odoov1.OdooBackupStatus{Phase: phase},
	}
}

func bqScheduled(name, instance, phase string, created time.Time) odoov1.OdooBackup {
	b := bqBackup(name, instance, phase, created)
	b.Spec.Schedule = bqSchedule
	return b
}

func TestBackupDecideStart(t *testing.T) {
	cand := bqBackup(bqSecond, bqInstA, odoov1.BackupPhasePending, bqT0.Add(time.Minute))
	cases := []struct {
		name    string
		all     []odoov1.OdooBackup
		mayRun  bool
		blocker string
	}{
		{"alone may start", nil, true, ""},
		{"other instance ignored", []odoov1.OdooBackup{
			bqBackup(bqFirst, bqInstB, odoov1.BackupPhaseRunning, bqT0),
		}, true, ""},
		{"running sibling blocks", []odoov1.OdooBackup{
			bqBackup(bqFirst, bqInstA, odoov1.BackupPhaseRunning, bqT0),
		}, false, bqFirst},
		{"older pending blocks", []odoov1.OdooBackup{
			bqBackup(bqFirst, bqInstA, odoov1.BackupPhasePending, bqT0),
		}, false, bqFirst},
		{"newer pending does not block", []odoov1.OdooBackup{
			bqBackup(bqFirst, bqInstA, odoov1.BackupPhasePending, bqT0.Add(2*time.Minute)),
		}, true, ""},
		{"terminal does not block", []odoov1.OdooBackup{
			bqBackup(bqFirst, bqInstA, odoov1.BackupPhaseSucceeded, bqT0),
			bqBackup(bqCron, bqInstA, odoov1.BackupPhaseFailed, bqT0),
		}, true, ""},
		{"scheduled sibling never blocks", []odoov1.OdooBackup{
			bqScheduled(bqFirst, bqInstA, odoov1.BackupPhaseRunning, bqT0),
		}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := backupDecideStart(&cand, tc.all)
			if d.MayStart != tc.mayRun || d.BlockedBy != tc.blocker {
				t.Errorf("decision = %+v, want mayStart=%v blockedBy=%q", d, tc.mayRun, tc.blocker)
			}
		})
	}
}

func TestBackupDecideStartTieBreaksByName(t *testing.T) {
	a := bqBackup(bqTieFirst, bqInstA, odoov1.BackupPhasePending, bqT0)
	b := bqBackup(bqTieSecond, bqInstA, odoov1.BackupPhasePending, bqT0)
	if d := backupDecideStart(&b, []odoov1.OdooBackup{a}); d.MayStart {
		t.Errorf("same timestamp: the lexically smaller name must go first")
	}
	if d := backupDecideStart(&a, []odoov1.OdooBackup{b}); !d.MayStart {
		t.Errorf("the lexically smaller name may start")
	}
}
