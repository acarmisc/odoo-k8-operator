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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:validation:XValidation:rule="!has(self.replicas) || self.replicas <= 1 || (has(self.dataVolume) && has(self.dataVolume.accessMode) && self.dataVolume.accessMode == 'ReadWriteMany')",message="replicas > 1 requires dataVolume.accessMode=ReadWriteMany (the filestore must be shared by all pods)"
// +kubebuilder:validation:XValidation:rule="!has(self.replicas) || self.replicas <= 1 || (has(self.addonsVolume) && has(self.addonsVolume.accessMode) && self.addonsVolume.accessMode == 'ReadWriteMany')",message="replicas > 1 requires addonsVolume.accessMode=ReadWriteMany"
type OdooInstanceSpec struct {
	Image string `json:"image,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Replicas  int32                       `json:"replicas,omitempty"`
	Version   string                      `json:"version,omitempty"`
	Edition   string                      `json:"edition,omitempty"`
	Addons    AddonVolumeSpec             `json:"addonsVolume,omitempty"`
	Data      DataVolumeSpec              `json:"dataVolume,omitempty"`
	Postgres  PostgresSpec                `json:"postgres,omitempty"`
	Config    map[string]string           `json:"config,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// AdminPasswordSecret names a Secret (key "password") holding Odoo's master
	// (database manager) password. It is injected as an environment variable and
	// passed to Odoo via --admin-passwd, never written to the ConfigMap.
	AdminPasswordSecret string `json:"adminPasswordSecret,omitempty"`
	// Upgrade makes the operator run `odoo -u <modules>` (via an OdooUpgrade) whenever the
	// set of synced addon commits changes, and only roll the pods out once it succeeded.
	Upgrade *UpgradeSpec `json:"upgrade,omitempty"`
	// Initialize makes the operator create and initialise the database once, through an
	// OdooUpgrade named <name>-init, before any pod is started. The Deployment stays at zero
	// replicas until that succeeds. It is set at creation time only.
	Initialize *InitializeSpec `json:"initialize,omitempty"`
}

// InitializeSpec describes the one-time database initialisation of an OdooInstance:
// `odoo -d <database> -i <modules> --stop-after-init`, then the password of the Odoo
// "admin" user is set through `odoo shell`.
// +kubebuilder:validation:XValidation:rule="size(self.adminUserPasswordSecret) > 0",message="adminUserPasswordSecret is required"
type InitializeSpec struct {
	// Modules to install with -i. ["base"] is the minimal value and creates an empty Odoo database.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9_]+$`
	Modules []string `json:"modules"`
	// WithDemo loads demo data. Defaults to false, which passes --without-demo=all.
	WithDemo bool `json:"withDemo,omitempty"`
	// AdminUserPasswordSecret names a Secret in the same namespace (key "password") whose
	// value becomes the password of the Odoo user "admin". Required.
	// +kubebuilder:validation:MinLength=1
	AdminUserPasswordSecret string `json:"adminUserPasswordSecret"`
	// TimeoutSeconds bounds the initialisation Job. Defaults to 1800.
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`
}

// UpgradeSpec configures automatic module upgrades for an OdooInstance.
type UpgradeSpec struct {
	// Modules to update with -u after addon commits change. Use ["all"] for every module.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9_]+$`
	Modules []string `json:"modules"`
	// TimeoutSeconds for each generated OdooUpgrade. Defaults to 1800.
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`
}

type AddonVolumeSpec struct {
	StorageClass *string `json:"storageClass,omitempty"`
	Size         string  `json:"size,omitempty"`
	// AccessMode of the PVC. Defaults to ReadWriteOnce; use ReadWriteMany when
	// running more than one replica.
	// +kubebuilder:validation:Enum=ReadWriteOnce;ReadWriteMany
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`
}

// DataVolumeSpec configures persistent storage for Odoo's data_dir
// (filestore, sessions) mounted at /var/lib/odoo. Without this, attachments
// such as product images live on the pod's ephemeral filesystem and are
// lost on every restart.
type DataVolumeSpec struct {
	StorageClass *string `json:"storageClass,omitempty"`
	Size         string  `json:"size,omitempty"`
	// AccessMode of the PVC. Defaults to ReadWriteOnce; use ReadWriteMany when
	// running more than one replica.
	// +kubebuilder:validation:Enum=ReadWriteOnce;ReadWriteMany
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`
	// RetainOnDelete keeps the data PVC (filestore) when the OdooInstance is
	// deleted. Defaults to true; set to false to garbage-collect it with the instance.
	RetainOnDelete *bool `json:"retainOnDelete,omitempty"`
}

type PostgresSpec struct {
	Host string `json:"host,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port           int32  `json:"port,omitempty"`
	Database       string `json:"database,omitempty"`
	User           string `json:"user,omitempty"`
	PasswordSecret string `json:"passwordSecret,omitempty"`
}

type OdooInstanceStatus struct {
	Ready              bool     `json:"ready,omitempty"`
	Phase              string   `json:"phase,omitempty"`
	AddonPaths         []string `json:"addonPaths,omitempty"`
	ObservedGeneration int64    `json:"observedGeneration,omitempty"`
	// Conditions hold Ready, Progressing and Degraded. The reasons are listed in docs/odooinstance.md.
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchStrategy=merge
	// +patchMergeKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// Message is a human summary of the current state, e.g. the reason for Degraded.
	Message string `json:"message,omitempty"`
	// Initialized is true once the database initialisation of spec.initialize has
	// succeeded, or was skipped because the instance already served traffic.
	Initialized bool `json:"initialized,omitempty"`
	// InitUpgrade names the OdooUpgrade doing the database initialisation, while it runs
	// or after it failed.
	InitUpgrade string `json:"initUpgrade,omitempty"`
	// AppliedAddonsHash is the addons hash the running pods were rolled out with. With
	// spec.upgrade set it only advances after the matching OdooUpgrade succeeded.
	AppliedAddonsHash string `json:"appliedAddonsHash,omitempty"`
	// ActiveUpgrade names the OdooUpgrade currently gating a rollout, if any.
	ActiveUpgrade string `json:"activeUpgrade,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".spec.version"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

type OdooInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="!has(self.initialize) || has(oldSelf.initialize)",message="spec.initialize cannot be added to an existing OdooInstance: it would re-initialise a live database"
	Spec   OdooInstanceSpec   `json:"spec,omitempty"`
	Status OdooInstanceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type OdooInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OdooInstance `json:"items"`
}

func init() {
	register(&OdooInstance{}, &OdooInstanceList{})
}
