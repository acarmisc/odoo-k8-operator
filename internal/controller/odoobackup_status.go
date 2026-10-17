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

// Pure mapping from an OdooBackup status to its conditions and events. The reconciler
// decides the phase; these functions only describe it.

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

// eventActionBackup is the Event action of backup transitions.
const eventActionBackup = "Backup"

// backupConditions maps the backup status to Succeeded, Progressing and Degraded. reason
// explains a Pending or Failed phase; when empty, a default is used.
func backupConditions(st odoov1.OdooBackupStatus, reason string) []metav1.Condition {
	switch st.Phase {
	case odoov1.BackupPhaseSucceeded:
		msg := "backup completed"
		if st.LastBackup != "" {
			msg = "backup completed: " + st.LastBackup
		}
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeSucceeded, condTrue, odoov1.ReasonComplete, msg),
			newCondition(odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonComplete, "backup finished"),
			newCondition(odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, "no backup errors"),
		}
	case odoov1.BackupPhaseRunning:
		msg := fmt.Sprintf("backup Job %s is running", st.JobName)
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeSucceeded, condUnknown, odoov1.ReasonRunning, msg),
			newCondition(odoov1.ConditionTypeProgressing, condTrue, odoov1.ReasonRunning, msg),
			newCondition(odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, "no backup errors"),
		}
	case odoov1.BackupPhaseFailed:
		if reason == "" {
			reason = odoov1.ReasonJobFailed
		}
		msg := st.Message
		if msg == "" {
			msg = "backup failed"
		}
		return []metav1.Condition{
			newCondition(odoov1.ConditionTypeSucceeded, condFalse, reason, msg),
			newCondition(odoov1.ConditionTypeProgressing, condFalse, reason, "backup stopped"),
			newCondition(odoov1.ConditionTypeDegraded, condTrue, reason, msg),
		}
	case odoov1.BackupPhaseScheduled:
		succeeded := newCondition(odoov1.ConditionTypeSucceeded, condUnknown, odoov1.ReasonScheduled,
			"no scheduled backup has completed yet")
		if st.LastSuccessfulTime != nil {
			succeeded = newCondition(odoov1.ConditionTypeSucceeded, condTrue, odoov1.ReasonComplete,
				"last scheduled backup succeeded")
		}
		return []metav1.Condition{
			succeeded,
			newCondition(odoov1.ConditionTypeProgressing, condFalse, odoov1.ReasonScheduled,
				"CronJob "+st.CronJobName+" runs the backups"),
			newCondition(odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, "no backup errors"),
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
			newCondition(odoov1.ConditionTypeDegraded, condFalse, odoov1.ReasonAsExpected, "no backup errors"),
		}
	}
}

// backupEvents lists the events for a backup status change.
func backupEvents(before, after odoov1.OdooBackupStatus) []statusEvent {
	var evs []statusEvent
	if after.Phase == odoov1.BackupPhaseRunning && before.Phase != odoov1.BackupPhaseRunning {
		evs = append(evs, statusEvent{
			Type: corev1.EventTypeNormal, Reason: eventReasonStarted, Action: eventActionBackup,
			Message: fmt.Sprintf("backup Job %s started", after.JobName),
		})
	}
	if after.Phase == odoov1.BackupPhaseSucceeded && before.Phase != odoov1.BackupPhaseSucceeded {
		msg := "backup Job completed"
		if after.LastBackup != "" {
			msg += ": " + after.LastBackup
		}
		evs = append(evs, statusEvent{
			Type: corev1.EventTypeNormal, Reason: eventReasonSucceeded, Action: eventActionBackup, Message: msg,
		})
	}
	if after.Phase == odoov1.BackupPhaseFailed && (before.Phase != odoov1.BackupPhaseFailed || before.Message != after.Message) {
		msg := after.Message
		if msg == "" {
			msg = "backup failed"
		}
		evs = append(evs, statusEvent{
			Type: corev1.EventTypeWarning, Reason: eventReasonFailed, Action: eventActionBackup, Message: msg,
		})
	}
	return evs
}
