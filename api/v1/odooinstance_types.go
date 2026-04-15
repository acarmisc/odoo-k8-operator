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

type OdooInstanceSpec struct {
	Image     string                      `json:"image,omitempty"`
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Replicas  int32                       `json:"replicas,omitempty"`
	Version   string                      `json:"version,omitempty"`
	Edition   string                      `json:"edition,omitempty"`
	Addons    AddonVolumeSpec             `json:"addonsVolume,omitempty"`
	Postgres  PostgresSpec                `json:"postgres,omitempty"`
	Config    map[string]string           `json:"config,omitempty"`
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

type AddonVolumeSpec struct {
	StorageClass *string `json:"storageClass,omitempty"`
	Size         string  `json:"size,omitempty"`
}

type PostgresSpec struct {
	Host           string `json:"host,omitempty"`
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
	SchemeBuilder.Register(&OdooInstance{}, &OdooInstanceList{})
}
