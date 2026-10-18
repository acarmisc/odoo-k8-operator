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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

func TestDataRetainOnDelete(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name string
		ret  *bool
		want bool
	}{
		{"nil defaults to retain", nil, true},
		{"explicit true retains", &yes, true},
		{"explicit false garbage-collects", &no, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := &odoov1.OdooInstanceSpec{Data: odoov1.DataVolumeSpec{RetainOnDelete: tc.ret}}
			if got := dataRetainOnDelete(spec); got != tc.want {
				t.Errorf("dataRetainOnDelete() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAccessModesOrDefault(t *testing.T) {
	tests := []struct {
		name string
		mode corev1.PersistentVolumeAccessMode
		want []corev1.PersistentVolumeAccessMode
	}{
		{"unset defaults to ReadWriteOnce", "", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}},
		{"ReadWriteMany is honored", corev1.ReadWriteMany, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := accessModesOrDefault(tc.mode); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("accessModesOrDefault(%q) = %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}

func TestPVCNames(t *testing.T) {
	if got := addonsPVCName("odoo1"); got != "odoo1-addons" {
		t.Errorf("addonsPVCName = %q", got)
	}
	if got := dataPVCName("odoo1"); got != "odoo1-data" {
		t.Errorf("dataPVCName = %q", got)
	}
	if got := configMapName("odoo1"); got != "odoo1-config" {
		t.Errorf("configMapName = %q", got)
	}
}
