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
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	testAdminSecret = "odoo-admin"
	testPGSecret    = "odoo-pg"
)

func TestDeploymentStrategy(t *testing.T) {
	zero := intstr.FromInt32(0)
	one := intstr.FromInt32(1)
	rolling := &appsv1.RollingUpdateDeployment{MaxUnavailable: &zero, MaxSurge: &one}

	tests := []struct {
		name     string
		replicas int32
		mode     corev1.PersistentVolumeAccessMode
		want     appsv1.DeploymentStrategy
	}{
		{
			name:     "single RWO replica recreates",
			replicas: 1,
			mode:     corev1.ReadWriteOnce,
			want:     appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
		},
		{
			name:     "unset access mode is treated as RWO",
			replicas: 1,
			mode:     "",
			want:     appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
		},
		{
			name:     "single RWX replica rolls",
			replicas: 1,
			mode:     corev1.ReadWriteMany,
			want:     appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: rolling},
		},
		{
			name:     "multiple replicas roll with zero unavailable",
			replicas: 3,
			mode:     corev1.ReadWriteMany,
			want:     appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: rolling},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := deploymentStrategy(tc.replicas, tc.mode)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("deploymentStrategy() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestDesiredReplicas(t *testing.T) {
	tests := map[string]struct {
		spec int32
		want int32
	}{
		"unset":    {0, 1},
		"negative": {-2, 1},
		"explicit": {3, 3},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			spec := &odoov1.OdooInstanceSpec{Replicas: tc.spec}
			if got := desiredReplicas(spec); got != tc.want {
				t.Errorf("desiredReplicas(%d) = %d, want %d", tc.spec, got, tc.want)
			}
		})
	}
}

func TestBuildWebArgs(t *testing.T) {
	plain := &odoov1.OdooInstanceSpec{}
	if got := buildWebArgs(plain); !reflect.DeepEqual(got, []string{upgradeFlagConfig, odooConfPath}) {
		t.Errorf("buildWebArgs() without admin secret = %v", got)
	}

	// Odoo has no command line flag for the master password, so the web container reads the
	// rendered config instead of passing an --admin-passwd argument.
	withAdmin := &odoov1.OdooInstanceSpec{AdminPasswordSecret: testAdminSecret}
	want := []string{upgradeFlagConfig, renderedConfPath}
	if got := buildWebArgs(withAdmin); !reflect.DeepEqual(got, want) {
		t.Errorf("buildWebArgs() with admin secret = %v, want %v", got, want)
	}
	for _, arg := range buildWebArgs(withAdmin) {
		if strings.Contains(arg, "admin-passwd") || strings.Contains(arg, envAdminPasswd) {
			t.Errorf("web args must not carry the master password, got %q", arg)
		}
	}
}

func envByName(env []corev1.EnvVar, name string) (corev1.EnvVar, bool) {
	for i := range env {
		if env[i].Name == name {
			return env[i], true
		}
	}
	return corev1.EnvVar{}, false
}

func TestBuildOdooEnv(t *testing.T) {
	tests := []struct {
		name      string
		spec      odoov1.OdooInstanceSpec
		wantNames []string
		wantRef   map[string]string // env name -> secret name
	}{
		{
			name:      "nothing configured",
			spec:      odoov1.OdooInstanceSpec{},
			wantNames: nil,
		},
		{
			name:      "postgres password feeds both libpq and legacy env",
			spec:      odoov1.OdooInstanceSpec{Postgres: odoov1.PostgresSpec{PasswordSecret: testPGSecret, Database: testPGDatabase}},
			wantNames: []string{envPGPassword, envDBPassword, envPGSSLMode},
			wantRef:   map[string]string{envPGPassword: testPGSecret, envDBPassword: testPGSecret},
		},
		{
			name:      "admin password is not in the Odoo containers env",
			spec:      odoov1.OdooInstanceSpec{AdminPasswordSecret: testAdminSecret},
			wantNames: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := buildOdooEnv(&tc.spec)
			var names []string
			for _, e := range env {
				names = append(names, e.Name)
			}
			if !reflect.DeepEqual(names, tc.wantNames) {
				t.Fatalf("env names = %v, want %v", names, tc.wantNames)
			}
			for envName, secretName := range tc.wantRef {
				e, ok := envByName(env, envName)
				if !ok || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
					t.Fatalf("env %s is not a secret reference", envName)
				}
				ref := e.ValueFrom.SecretKeyRef
				if ref.Name != secretName || ref.Key != secretKeyPassword {
					t.Errorf("env %s references %s/%s, want %s/%s", envName, ref.Name, ref.Key, secretName, secretKeyPassword)
				}
			}
		})
	}
}

func TestOdooProbes(t *testing.T) {
	startup, readiness, liveness := odooProbes()

	check := func(name string, p *corev1.Probe, period, failure int32) {
		t.Helper()
		if p.HTTPGet == nil || p.HTTPGet.Path != healthPath {
			t.Errorf("%s probe must use HTTP path %s", name, healthPath)
		}
		if p.HTTPGet != nil && p.HTTPGet.Port != intstr.FromInt32(portOdoo) {
			t.Errorf("%s probe must target port %d", name, portOdoo)
		}
		if p.PeriodSeconds != period {
			t.Errorf("%s periodSeconds = %d, want %d", name, p.PeriodSeconds, period)
		}
		if p.FailureThreshold != failure {
			t.Errorf("%s failureThreshold = %d, want %d", name, p.FailureThreshold, failure)
		}
	}
	check("startup", startup, 10, 30)
	check("readiness", readiness, 10, 3)
	check("liveness", liveness, 20, 3)
}

func TestColocationAffinity(t *testing.T) {
	tests := []struct {
		name       string
		data, addr corev1.PersistentVolumeAccessMode
		wantTerm   bool
	}{
		{name: "both unset is RWO", wantTerm: true},
		{name: "both RWO", data: cronTestRWOAccess, addr: cronTestRWOAccess, wantTerm: true},
		{name: "RWO data, RWX addons", data: cronTestRWOAccess, addr: cronTestRWXAccess, wantTerm: true},
		{name: "RWX data, RWO addons", data: cronTestRWXAccess, addr: cronTestRWOAccess, wantTerm: true},
		{name: "both RWX needs no affinity", data: cronTestRWXAccess, addr: cronTestRWXAccess, wantTerm: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := odoov1.OdooInstanceSpec{
				Data:   odoov1.DataVolumeSpec{AccessMode: tc.data},
				Addons: odoov1.AddonVolumeSpec{AccessMode: tc.addr},
			}
			aff := colocationAffinity(testInstanceName, &spec)
			if !tc.wantTerm {
				if aff != nil {
					t.Fatalf("colocationAffinity() = %+v, want nil for ReadWriteMany volumes", aff)
				}
				return
			}
			if aff == nil || aff.PodAffinity == nil {
				t.Fatalf("colocationAffinity() has no pod affinity")
			}
			terms := aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution
			if len(terms) != 1 {
				t.Fatalf("required affinity terms = %d, want 1", len(terms))
			}
			if terms[0].TopologyKey != corev1.LabelHostname {
				t.Errorf("topologyKey = %q, want %q", terms[0].TopologyKey, corev1.LabelHostname)
			}
			want := map[string]string{labelInstance: testInstanceName}
			if !reflect.DeepEqual(terms[0].LabelSelector.MatchLabels, want) {
				t.Errorf("affinity selector = %v, want %v", terms[0].LabelSelector.MatchLabels, want)
			}
		})
	}
}

// Both templates must carry the same required term, and each template's own labels must match
// it. A pod that matches its own term may start when no other pod matches, which is how the
// first pod of an instance gets scheduled.
func TestColocationSelectorMatchesBothTemplates(t *testing.T) {
	instance := cronTestInstanceObj(func(i *odoov1.OdooInstance) { i.Spec.Cron = cronTestSpec() })
	templates := []struct {
		name string
		tmpl corev1.PodTemplateSpec
	}{
		{name: "web", tmpl: buildPodTemplate(instance, cronTestHashes())},
		{name: "cron", tmpl: buildCronPodTemplate(instance, cronTestHashes())},
	}
	for _, tc := range templates {
		if tc.tmpl.Spec.Affinity == nil || tc.tmpl.Spec.Affinity.PodAffinity == nil {
			t.Fatalf("%s pod template has no pod affinity for RWO volumes", tc.name)
		}
	}
	if !reflect.DeepEqual(templates[0].tmpl.Spec.Affinity, templates[1].tmpl.Spec.Affinity) {
		t.Errorf("web affinity %+v differs from cron affinity %+v",
			templates[0].tmpl.Spec.Affinity, templates[1].tmpl.Spec.Affinity)
	}

	term := templates[0].tmpl.Spec.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
	selector := labels.SelectorFromSet(term.LabelSelector.MatchLabels)
	for _, tc := range templates {
		if !selector.Matches(labels.Set(tc.tmpl.Labels)) {
			t.Errorf("%s pod labels %v do not match affinity selector %v", tc.name, tc.tmpl.Labels, selector)
		}
	}
	// The web Deployment selector stays app=odoo, so the Service and PDB are unaffected.
	if !labels.SelectorFromSet(selectorLabels(cronTestInstance)).Matches(labels.Set(templates[0].tmpl.Labels)) {
		t.Errorf("web pod labels no longer match the web selector")
	}
}

func TestBuildPodTemplateHardening(t *testing.T) {
	instance := &odoov1.OdooInstance{}
	instance.Name = testInstanceName
	hashes := rolloutHashes{config: "c", secrets: "s", addons: "a"}

	tmpl := buildPodTemplate(instance, hashes)

	if got := tmpl.Annotations[annotationConfigHash]; got != "c" {
		t.Errorf("config-hash annotation = %q, want c", got)
	}
	if got := tmpl.Annotations[annotationSecretsHash]; got != "s" {
		t.Errorf("secrets-hash annotation = %q, want s", got)
	}
	if got := tmpl.Annotations[annotationAddonsHash]; got != "a" {
		t.Errorf("addons-hash annotation = %q, want a", got)
	}
	if got := tmpl.Labels[labelAppName]; got != appOdoo {
		t.Errorf("app.kubernetes.io/name = %q, want %s", got, appOdoo)
	}
	if got := tmpl.Labels[labelManagedBy]; got != managedByOperator {
		t.Errorf("app.kubernetes.io/managed-by = %q, want %s", got, managedByOperator)
	}
	if got := tmpl.Labels[labelInstance]; got != testInstanceName {
		t.Errorf("legacy instance label = %q, want %s", got, testInstanceName)
	}
	if tmpl.Spec.AutomountServiceAccountToken == nil || *tmpl.Spec.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken must be false")
	}
	if tmpl.Spec.TerminationGracePeriodSeconds == nil || *tmpl.Spec.TerminationGracePeriodSeconds != 60 {
		t.Errorf("terminationGracePeriodSeconds must be 60")
	}

	container := tmpl.Spec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil ||
		!*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("container must run with a read-only root filesystem")
	}
	mounts := map[string]string{}
	for _, m := range container.VolumeMounts {
		mounts[m.Name] = m.MountPath
	}
	if mounts[volumeTmp] != mountPathTmp {
		t.Errorf("emptyDir %s must be mounted at %s", volumeTmp, mountPathTmp)
	}
	if !strings.HasPrefix(mounts[volumeData], mountPathData) {
		t.Errorf("data volume must be mounted at %s", mountPathData)
	}

	ports := map[string]int32{}
	for _, p := range container.Ports {
		ports[p.Name] = p.ContainerPort
	}
	if ports[portNameOdoo] != 8069 || ports[portNameLongpolling] != 8072 {
		t.Errorf("container ports = %v, want odoo=8069 longpolling=8072", ports)
	}
}

func TestBuildServicePorts(t *testing.T) {
	ports := buildServicePorts()
	if len(ports) != 2 {
		t.Fatalf("buildServicePorts() returned %d ports, want 2", len(ports))
	}
	lp := ports[1]
	if lp.Name != portNameLongpolling || lp.Port != 8072 {
		t.Errorf("second service port = %s:%d, want longpolling:8072", lp.Name, lp.Port)
	}
}
