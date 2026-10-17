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
	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

// backupQueueDecision says whether a Pending one-shot backup may start its Job now.
type backupQueueDecision struct {
	MayStart bool
	// BlockedBy names the backup holding the instance (Running) or queued ahead of it
	// (oldest Pending). Empty when MayStart is true.
	BlockedBy string
}

// backupDecideStart serializes one-shot backups per instance. all is the list of
// OdooBackups in the candidate's namespace; scheduled backups (spec.schedule set) are
// skipped because their CronJob runs on its own schedule. The candidate may start only
// when no one-shot sibling is Running and no one-shot sibling Pending backup is older
// (creationTimestamp, then name). Terminal siblings never block.
func backupDecideStart(candidate *odoov1.OdooBackup, all []odoov1.OdooBackup) backupQueueDecision {
	instance := candidate.Spec.InstanceRef.Name
	for i := range all {
		other := &all[i]
		if other.Name == candidate.Name || other.Spec.InstanceRef.Name != instance || other.Spec.Schedule != "" {
			continue
		}
		switch other.Status.Phase {
		case odoov1.BackupPhaseRunning:
			return backupQueueDecision{BlockedBy: other.Name}
		case "", odoov1.BackupPhasePending:
			if backupOlder(other, candidate) {
				return backupQueueDecision{BlockedBy: other.Name}
			}
		}
	}
	return backupQueueDecision{MayStart: true}
}

// backupOlder orders backups by creation time, breaking ties by name.
func backupOlder(a, b *odoov1.OdooBackup) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}
