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

package v1

// Condition types written to status.conditions. Each kind uses the subset it needs:
// OdooInstance and OdooAddon use Ready, Progressing and Degraded (OdooInstance also CronReady); OdooUpgrade and
// OdooBackup use Succeeded and Progressing (OdooBackup also Degraded for a rejected CronJob).
const (
	// ConditionTypeReady is True when the object serves or is synced, and False otherwise.
	ConditionTypeReady = "Ready"
	// ConditionTypeProgressing is True while the operator is actively moving the object
	// toward its desired state (rollout, sync, upgrade job).
	ConditionTypeProgressing = "Progressing"
	// ConditionTypeDegraded is True when the operator hit an error it could not clear.
	ConditionTypeDegraded = "Degraded"
	// ConditionTypeSucceeded is True when the upgrade completed, False when it failed, and
	// Unknown while it has not finished yet.
	ConditionTypeSucceeded = "Succeeded"
	// ConditionTypeCronReady reports the dedicated cron Deployment of an OdooInstance. It is
	// present only while spec.cron is enabled and does not affect Ready.
	ConditionTypeCronReady = "CronReady"
)

// Condition reasons. They are stable identifiers meant for scripts and alerts; the
// condition message carries the human-readable detail.
const (
	// Reasons used by OdooInstance.
	ReasonAllReplicasReady   = "AllReplicasReady"
	ReasonDeploymentNotReady = "DeploymentNotReady"
	ReasonReconciling        = "Reconciling"
	ReasonReconciled         = "Reconciled"
	ReasonAsExpected         = "AsExpected"
	ReasonReconcileError     = "ReconcileError"
	ReasonUpgrading          = "Upgrading"
	ReasonUpgradeFailed      = "UpgradeFailed"
	ReasonMissingSecret      = "MissingSecret"
	ReasonInvalidConfig      = "InvalidConfig"
	// ReasonInitializing: spec.initialize's OdooUpgrade is creating the database; pods stay at zero.
	ReasonInitializing = "Initializing"
	// ReasonInitializationFailed: the initialisation OdooUpgrade failed; pods stay at zero.
	ReasonInitializationFailed = "InitializationFailed"

	// Reasons used by the cron Deployment (CronReady).
	// ReasonCronRunning: the cron pod is ready.
	ReasonCronRunning = "CronRunning"
	// ReasonCronNotReady: the cron pod exists but is not ready.
	ReasonCronNotReady = "CronNotReady"
	// ReasonCronPending: no cron pod exists yet (not created, held during initialisation, or
	// waiting for a web pod on the same node when the volumes are ReadWriteOnce).
	ReasonCronPending = "CronPending"

	// Reasons used by OdooAddon.
	ReasonSynced           = "Synced"
	ReasonSyncing          = "Syncing"
	ReasonPending          = "Pending"
	ReasonInstanceNotFound = "InstanceNotFound"
	ReasonVolumeNotBound   = "VolumeNotBound"
	ReasonSyncFailed       = "SyncFailed"
	ReasonInvalidSpec      = "InvalidSpec"
	ReasonModuleConflict   = "ModuleConflict"
	ReasonJobConflict      = "JobConflict"

	// Reasons used by OdooUpgrade.
	ReasonRunning   = "Running"
	ReasonComplete  = "Complete"
	ReasonJobFailed = "JobFailed"

	// Reasons used by OdooBackup.
	ReasonScheduled       = "Scheduled"
	ReasonCronJobRejected = "CronJobRejected"
)
