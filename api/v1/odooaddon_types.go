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

type OdooAddonSpec struct {
	GitUrl      string          `json:"gitUrl"`
	GitRef      string          `json:"gitRef,omitempty"`
	AddonPath   string          `json:"addonPath,omitempty"`
	SingleAddon bool            `json:"singleAddon,omitempty"`
	InstanceRef OdooInstanceRef `json:"instanceRef"`
	ReadOnly    bool            `json:"readOnly,omitempty"`
}

type OdooInstanceRef struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
}

type OdooAddonStatus struct {
	Ready        bool         `json:"ready,omitempty"`
	Phase        string       `json:"phase,omitempty"`
	ClonedCommit string       `json:"clonedCommit,omitempty"`
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=".status.ready"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Instance",type="string",JSONPath=".spec.instanceRef.name"
// +kubebuilder:printcolumn:name="Git URL",type="string",JSONPath=".spec.gitUrl"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

type OdooAddon struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OdooAddonSpec   `json:"spec,omitempty"`
	Status OdooAddonStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type OdooAddonList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OdooAddon `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OdooAddon{}, &OdooAddonList{})
}
