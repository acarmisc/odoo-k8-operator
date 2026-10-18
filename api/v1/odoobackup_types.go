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

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phases reported in OdooBackupStatus.Phase. One-shot backups use Pending, Running,
// Succeeded and Failed. Scheduled backups use Scheduled, or Failed when the CronJob
// was rejected.
const (
	BackupPhasePending   = "Pending"
	BackupPhaseRunning   = "Running"
	BackupPhaseSucceeded = "Succeeded"
	BackupPhaseFailed    = "Failed"
	BackupPhaseScheduled = "Scheduled"
)

// OdooBackupSpec describes a backup of one database of an OdooInstance: a pg_dump plus,
// optionally, a tar of the filestore. Without schedule the backup is one-shot and its
// spec is immutable. With schedule the operator keeps a CronJob in sync with the spec.
type OdooBackupSpec struct {
	// InstanceRef names the OdooInstance (same namespace) whose database and filestore
	// are backed up.
	InstanceRef OdooInstanceRef `json:"instanceRef"`
	// Database to dump. Defaults to the instance's postgres.database.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_.-]+$`
	Database string `json:"database,omitempty"`
	// IncludeFilestore also archives /var/lib/odoo/filestore/<database>. Defaults to true.
	// +kubebuilder:default=true
	IncludeFilestore *bool `json:"includeFilestore,omitempty"`
	// Destination where the backup files are written.
	Destination BackupDestination `json:"destination"`
	// Retention keeps the newest N backups of this database at the destination and deletes
	// older ones (the dump and its filestore archive). Unset keeps every backup.
	// +kubebuilder:validation:Minimum=1
	Retention *int32 `json:"retention,omitempty"`
	// ScratchSize bounds the emptyDir that holds the dump before the upload to S3. It is
	// not used for a PVC destination. Defaults to 10Gi.
	// +kubebuilder:default="10Gi"
	// +kubebuilder:validation:Pattern=`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E|m)?$`
	ScratchSize string `json:"scratchSize,omitempty"`
	// TimeoutSeconds bounds each backup pod (activeDeadlineSeconds). Defaults to 3600.
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`
	// Schedule is a 5-field cron expression (minute hour day-of-month month day-of-week).
	// When set, the backup runs as a CronJob. Empty means a one-shot backup.
	// +kubebuilder:validation:Pattern=`^[^\s]+(\s+[^\s]+){4}$`
	Schedule string `json:"schedule,omitempty"`
	// Suspend pauses a scheduled backup. Ignored for one-shot backups.
	Suspend bool `json:"suspend,omitempty"`
}

// BackupDestination selects exactly one place to store backups.
// +kubebuilder:validation:XValidation:rule="has(self.pvc) != has(self.s3)",message="exactly one of pvc or s3 must be set"
type BackupDestination struct {
	// PVC writes to a PersistentVolumeClaim in the instance's namespace.
	PVC *BackupPVCDestination `json:"pvc,omitempty"`
	// S3 uploads to an S3 or S3-compatible bucket (AWS, MinIO, ...).
	S3 *BackupS3Destination `json:"s3,omitempty"`
}

// BackupPVCDestination is a claim written at its root or at subPath.
type BackupPVCDestination struct {
	// ClaimName is a PersistentVolumeClaim in the same namespace as the OdooBackup.
	// +kubebuilder:validation:MinLength=1
	ClaimName string `json:"claimName"`
	// SubPath is a directory inside the claim. It is created by the mount, not by the operator.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._/-]+$`
	SubPath string `json:"subPath,omitempty"`
}

// BackupS3Destination is a bucket prefix on an S3 API.
type BackupS3Destination struct {
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`
	// Prefix is a key prefix inside the bucket.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9._/-]+$`
	Prefix string `json:"prefix,omitempty"`
	// Endpoint is the S3 API URL for non-AWS services, for example https://minio.example.com.
	Endpoint string `json:"endpoint,omitempty"`
	// Region is passed as AWS_DEFAULT_REGION.
	Region string `json:"region,omitempty"`
	// CredentialsSecret names a Secret in the same namespace with keys accessKeyID and
	// secretAccessKey.
	// +kubebuilder:validation:MinLength=1
	CredentialsSecret string `json:"credentialsSecret"`
	// ForcePathStyle uses bucket/key URLs instead of virtual-hosted URLs. Most
	// S3-compatible services need it.
	ForcePathStyle bool `json:"forcePathStyle,omitempty"`
}

// OdooBackupStatus reports the state of a backup.
type OdooBackupStatus struct {
	// Phase is one of Pending, Running, Succeeded, Failed (one-shot) or Scheduled (CronJob).
	Phase string `json:"phase,omitempty"`
	// JobName is the Job executing a one-shot backup.
	JobName string `json:"jobName,omitempty"`
	// CronJobName is the CronJob executing a scheduled backup.
	CronJobName string `json:"cronJobName,omitempty"`
	// StartTime of the one-shot Job.
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// CompletionTime of the one-shot Job.
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// LastScheduleTime is copied from the CronJob status.
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
	// LastSuccessfulTime is copied from the CronJob status.
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`
	// LastBackup lists the files written by the last successful one-shot backup, for
	// example "shop-20261017T120000Z.dump,shop-20261017T120000Z-filestore.tar.gz".
	LastBackup string `json:"lastBackup,omitempty"`
	// Message carries the failure reason or the tail of the pod log.
	Message            string `json:"message,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	// Conditions hold Succeeded, Progressing and Degraded. The reasons are listed in docs/backups.md.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Instance",type="string",JSONPath=".spec.instanceRef.name"
// +kubebuilder:printcolumn:name="Schedule",type="string",JSONPath=".spec.schedule"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

type OdooBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// The spec of a one-shot backup is immutable. A scheduled backup may change, but it
	// cannot become one-shot again.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf || (has(oldSelf.schedule) && has(self.schedule))",message="a one-shot OdooBackup is immutable; create a new OdooBackup, or keep a schedule set"
	Spec   OdooBackupSpec   `json:"spec,omitempty"`
	Status OdooBackupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type OdooBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OdooBackup `json:"items"`
}

func init() {
	register(&OdooBackup{}, &OdooBackupList{})
}
