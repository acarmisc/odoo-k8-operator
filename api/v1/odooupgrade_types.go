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

// Phases reported in OdooUpgradeStatus.Phase.
const (
	UpgradePhasePending   = "Pending"
	UpgradePhaseRunning   = "Running"
	UpgradePhaseSucceeded = "Succeeded"
	UpgradePhaseFailed    = "Failed"
)

// OdooUpgradeSpec describes a one-shot `odoo -u/-i ... --stop-after-init` run against
// the database of an OdooInstance. The spec is immutable: to run another upgrade create
// a new OdooUpgrade.
type OdooUpgradeSpec struct {
	// InstanceRef names the OdooInstance (same namespace) whose image, config, addons and
	// database are used.
	InstanceRef OdooInstanceRef `json:"instanceRef"`
	// Modules to update with -u. Use ["all"] to update every installed module.
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9_]+$`
	Modules []string `json:"modules,omitempty"`
	// InstallModules to install with -i.
	// +kubebuilder:validation:MaxItems=100
	// +kubebuilder:validation:items:Pattern=`^[a-zA-Z0-9_]+$`
	InstallModules []string `json:"installModules,omitempty"`
	// Database to upgrade. Defaults to the instance's postgres.database.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z0-9_.-]+$`
	Database string `json:"database,omitempty"`
	// TimeoutSeconds bounds the whole upgrade Job (activeDeadlineSeconds). Defaults to 1800.
	// +kubebuilder:validation:Minimum=60
	// +kubebuilder:validation:Maximum=86400
	TimeoutSeconds *int64 `json:"timeoutSeconds,omitempty"`
	// WithDemo loads demo data when installModules is set. Defaults to false, which
	// passes --without-demo=True.
	WithDemo bool `json:"withDemo,omitempty"`
	// AdminUserPasswordSecret names a Secret in the same namespace (key "password"). After
	// installModules succeeded, the value becomes the password of the Odoo user "admin",
	// set through `odoo shell`. The value reaches the Job only as an environment variable.
	// Requires installModules.
	AdminUserPasswordSecret string `json:"adminUserPasswordSecret,omitempty"`
}

type OdooUpgradeStatus struct {
	// Phase is one of Pending, Running, Succeeded, Failed.
	Phase string `json:"phase,omitempty"`
	// JobName is the Kubernetes Job executing the upgrade.
	JobName string `json:"jobName,omitempty"`
	// Message carries the failure reason or the tail of the Job log.
	Message            string       `json:"message,omitempty"`
	StartTime          *metav1.Time `json:"startTime,omitempty"`
	CompletionTime     *metav1.Time `json:"completionTime,omitempty"`
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	// Conditions hold Succeeded and Progressing. The reasons are listed in docs/upgrades.md.
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
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

type OdooUpgrade struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable; create a new OdooUpgrade instead"
	// +kubebuilder:validation:XValidation:rule="size(self.modules) > 0 || size(self.installModules) > 0",message="at least one of modules or installModules is required"
	Spec   OdooUpgradeSpec   `json:"spec,omitempty"`
	Status OdooUpgradeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type OdooUpgradeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OdooUpgrade `json:"items"`
}

func init() {
	register(&OdooUpgrade{}, &OdooUpgradeList{})
}
