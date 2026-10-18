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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	cronTestInstance   = "cron-unit"
	cronTestDeployName = "cron-unit-cron"
	cronTestImage      = "odoo:19.0"
	cronTestThreads    = int32(5)
	cronTestCPU        = "2"
	cronTestMemory     = "2Gi"
	cronTestNoHTTP     = "--no-http"
	cronTestMaxKey     = "max_cron_threads"
	cronTestHashCfg    = "cfg-hash"
	cronTestHashSec    = "sec-hash"
	cronTestHashAdd    = "add-hash"
	cronTestCondMsg    = "cron pod ready"
	cronTestNotReady   = "cron pod not ready: 0/1 replicas ready"
	cronTestCfgLine    = "max_cron_threads = 0\n"
	cronTestRWOAccess  = corev1.ReadWriteOnce
	cronTestRWXAccess  = corev1.ReadWriteMany
)

// cronTestSpec returns an enabled cron spec with the given worker count.
func cronTestSpec() *odoov1.CronSpec {
	return &odoov1.CronSpec{Enabled: true, MaxCronThreads: int32Ptr(cronTestThreads)}
}

func cronTestHashes() rolloutHashes {
	return rolloutHashes{config: cronTestHashCfg, secrets: cronTestHashSec, addons: cronTestHashAdd}
}

func cronTestInstanceObj(mutate func(*odoov1.OdooInstance)) *odoov1.OdooInstance {
	instance := &odoov1.OdooInstance{}
	instance.Name = cronTestInstance
	instance.Namespace = testNamespace
	if mutate != nil {
		mutate(instance)
	}
	return instance
}

// serviceSelects reports whether the Service selector matches the pod labels.
func serviceSelects(selector, podLabels map[string]string) bool {
	return labels.SelectorFromSet(selector).Matches(labels.Set(podLabels))
}

func TestCronNamingAndLabels(t *testing.T) {
	if got := cronDeploymentName(cronTestInstance); got != cronTestDeployName {
		t.Errorf("cronDeploymentName() = %q, want %q", got, cronTestDeployName)
	}

	sel := cronSelectorLabels(cronTestInstance)
	want := map[string]string{labelApp: appOdooCron, labelInstance: cronTestInstance}
	if !reflect.DeepEqual(sel, want) {
		t.Errorf("cronSelectorLabels() = %v, want %v", sel, want)
	}

	pod := cronPodLabels(cronTestInstance)
	for k, v := range map[string]string{
		labelAppName:     appOdoo,
		labelAppInstance: cronTestInstance,
		labelManagedBy:   managedByOperator,
		labelComponent:   componentCron,
	} {
		if pod[k] != v {
			t.Errorf("cron pod label %s = %q, want %q", k, pod[k], v)
		}
	}
}

func TestCronPodsNeverSelectedByWebSelectors(t *testing.T) {
	cronPod := cronPodLabels(cronTestInstance)
	webPod := podLabels(cronTestInstance)

	// The web Service selects app=odoo and instance=<name>; the web PDB selects podLabels.
	if serviceSelects(selectorLabels(cronTestInstance), cronPod) {
		t.Errorf("web Service selector %v matches cron pod labels %v", selectorLabels(cronTestInstance), cronPod)
	}
	if serviceSelects(podLabels(cronTestInstance), cronPod) {
		t.Errorf("web PDB selector matches cron pod labels %v", cronPod)
	}
	// The cron Deployment selects its own pods and not the web pods.
	if !serviceSelects(cronSelectorLabels(cronTestInstance), cronPod) {
		t.Errorf("cron selector %v does not match cron pod labels %v", cronSelectorLabels(cronTestInstance), cronPod)
	}
	if serviceSelects(cronSelectorLabels(cronTestInstance), webPod) {
		t.Errorf("cron selector matches web pod labels %v", webPod)
	}
}

func TestCronMaxThreadsAndArgs(t *testing.T) {
	tests := []struct {
		name string
		spec *odoov1.CronSpec
		want int32
	}{
		{name: "cron unset", spec: nil, want: 2},
		{name: "threads unset", spec: &odoov1.CronSpec{Enabled: true}, want: 2},
		{name: "threads set", spec: cronTestSpec(), want: cronTestThreads},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := &odoov1.OdooInstanceSpec{Cron: tc.spec}
			if got := cronMaxThreads(spec); got != tc.want {
				t.Errorf("cronMaxThreads() = %d, want %d", got, tc.want)
			}
		})
	}

	plain := cronTestInstanceObj(func(i *odoov1.OdooInstance) { i.Spec.Cron = cronTestSpec() })
	want := []string{upgradeFlagConfig, odooConfPath, cronTestNoHTTP, "--max-cron-threads=5"}
	if got := buildCronArgs(plain); !reflect.DeepEqual(got, want) {
		t.Errorf("buildCronArgs() = %v, want %v", got, want)
	}

	withAdmin := cronTestInstanceObj(func(i *odoov1.OdooInstance) {
		i.Spec.AdminPasswordSecret = testAdminSecret
		i.Spec.Cron = cronTestSpec()
	})
	wantAdmin := []string{upgradeFlagConfig, odooConfPath, adminPasswordArg, cronTestNoHTTP, "--max-cron-threads=5"}
	if got := buildCronArgs(withAdmin); !reflect.DeepEqual(got, wantAdmin) {
		t.Errorf("buildCronArgs() with admin secret = %v, want %v", got, wantAdmin)
	}
}

func TestCronResourcesFallback(t *testing.T) {
	web := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cronTestCPU)},
	}
	cronRes := corev1.ResourceRequirements{
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(cronTestMemory)},
	}

	tests := []struct {
		name string
		spec odoov1.OdooInstanceSpec
		want corev1.ResourceRequirements
	}{
		{
			name: "cron resources win over web",
			spec: odoov1.OdooInstanceSpec{Resources: web, Cron: &odoov1.CronSpec{Enabled: true, Resources: &cronRes}},
			want: cronRes,
		},
		{
			name: "web resources when cron has none",
			spec: odoov1.OdooInstanceSpec{Resources: web, Cron: &odoov1.CronSpec{Enabled: true}},
			want: web,
		},
		{
			name: "empty cron resources fall back to web",
			spec: odoov1.OdooInstanceSpec{Resources: web, Cron: &odoov1.CronSpec{Enabled: true, Resources: &corev1.ResourceRequirements{}}},
			want: web,
		},
		{
			name: "defaults when nothing is set",
			spec: odoov1.OdooInstanceSpec{Cron: &odoov1.CronSpec{Enabled: true}},
			want: odooResources(&odoov1.OdooInstanceSpec{}),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := cronResources(&tc.spec); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("cronResources() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCronReplicas(t *testing.T) {
	if got := cronReplicas(false); got != 1 {
		t.Errorf("cronReplicas(false) = %d, want 1", got)
	}
	if got := cronReplicas(true); got != 0 {
		t.Errorf("cronReplicas(true) = %d, want 0 while initialising", got)
	}
}

func TestBuildCronPodTemplate(t *testing.T) {
	instance := cronTestInstanceObj(func(i *odoov1.OdooInstance) {
		i.Spec.Cron = cronTestSpec()
	})
	hashes := cronTestHashes()
	cron := buildCronPodTemplate(instance, hashes)
	web := buildPodTemplate(instance, hashes)

	if len(cron.Spec.Containers) != 1 {
		t.Fatalf("cron containers = %d, want 1", len(cron.Spec.Containers))
	}
	c := cron.Spec.Containers[0]
	if len(c.Ports) != 0 {
		t.Errorf("cron container must expose no ports, got %v", c.Ports)
	}
	if c.StartupProbe != nil || c.ReadinessProbe != nil || c.LivenessProbe != nil {
		t.Errorf("cron container must have no HTTP probes")
	}
	if c.Image != cronTestImage {
		t.Errorf("cron image = %q, want %q", c.Image, cronTestImage)
	}
	if !containsAll(c.Args, cronTestNoHTTP, "--max-cron-threads=5") {
		t.Errorf("cron args = %v, want --no-http and --max-cron-threads=5", c.Args)
	}

	// Same rollout hashes as the web pods, so both templates roll together.
	for _, key := range []string{annotationConfigHash, annotationSecretsHash, annotationAddonsHash} {
		if cron.Annotations[key] != web.Annotations[key] {
			t.Errorf("annotation %s: cron %q, web %q", key, cron.Annotations[key], web.Annotations[key])
		}
	}
	if !reflect.DeepEqual(cron.Annotations, web.Annotations) {
		t.Errorf("cron annotations %v differ from web %v", cron.Annotations, web.Annotations)
	}

	if serviceSelects(selectorLabels(cronTestInstance), cron.Labels) {
		t.Errorf("cron pod labels %v are selected by the web Service", cron.Labels)
	}
	if *cron.Spec.AutomountServiceAccountToken || *cron.Spec.TerminationGracePeriodSeconds != terminationGraceSecs {
		t.Errorf("cron pod must not mount a service account token and must keep the web grace period")
	}
	if cron.Spec.Affinity == nil {
		t.Errorf("RWO volumes need the cron pod affinity to a web pod")
	}

	volumes := map[string]bool{}
	for _, v := range cron.Spec.Volumes {
		volumes[v.Name] = true
	}
	for _, v := range web.Spec.Volumes {
		if !volumes[v.Name] {
			t.Errorf("cron pod is missing web volume %q", v.Name)
		}
	}
}

func TestCronPodTemplateAffinityFollowsVolumes(t *testing.T) {
	rwx := cronTestInstanceObj(func(i *odoov1.OdooInstance) {
		i.Spec.Cron = cronTestSpec()
		i.Spec.Data.AccessMode = cronTestRWXAccess
		i.Spec.Addons.AccessMode = cronTestRWXAccess
	})
	if got := buildCronPodTemplate(rwx, cronTestHashes()).Spec.Affinity; got != nil {
		t.Errorf("ReadWriteMany cron pod has affinity %+v, want none", got)
	}
	if got := buildPodTemplate(rwx, cronTestHashes()).Spec.Affinity; got != nil {
		t.Errorf("ReadWriteMany web pod has affinity %+v, want none", got)
	}
}

func TestCronEnabled(t *testing.T) {
	tests := map[string]struct {
		spec *odoov1.CronSpec
		want bool
	}{
		"unset":    {nil, false},
		"disabled": {&odoov1.CronSpec{Enabled: false}, false},
		"enabled":  {&odoov1.CronSpec{Enabled: true}, true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := cronEnabled(&odoov1.OdooInstanceSpec{Cron: tc.spec}); got != tc.want {
				t.Errorf("cronEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCronCondition(t *testing.T) {
	tests := []struct {
		name       string
		out        cronOutcome
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    string
	}{
		{
			name:       "deployment not created",
			out:        cronOutcome{enabled: true},
			wantStatus: condFalse,
			wantReason: odoov1.ReasonCronPending,
		},
		{
			name:       "held during initialisation",
			out:        cronOutcome{enabled: true, deployFound: true, held: true},
			wantStatus: condFalse,
			wantReason: odoov1.ReasonCronPending,
		},
		{
			name:       "no pod yet",
			out:        cronOutcome{enabled: true, deployFound: true},
			wantStatus: condFalse,
			wantReason: odoov1.ReasonCronPending,
		},
		{
			name:       "pod exists but not ready",
			out:        cronOutcome{enabled: true, deployFound: true, replicas: 1},
			wantStatus: condFalse,
			wantReason: odoov1.ReasonCronNotReady,
			wantMsg:    cronTestNotReady,
		},
		{
			name:       "pod ready",
			out:        cronOutcome{enabled: true, deployFound: true, replicas: 1, ready: 1},
			wantStatus: condTrue,
			wantReason: odoov1.ReasonCronRunning,
			wantMsg:    cronTestCondMsg,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cronCondition(tc.out)
			if got.Type != odoov1.ConditionTypeCronReady || got.Status != tc.wantStatus || got.Reason != tc.wantReason {
				t.Fatalf("cronCondition() = %s/%s/%s, want CronReady/%s/%s",
					got.Type, got.Status, got.Reason, tc.wantStatus, tc.wantReason)
			}
			if tc.wantMsg != "" && got.Message != tc.wantMsg {
				t.Errorf("message = %q, want %q", got.Message, tc.wantMsg)
			}
		})
	}
}

func TestApplyCronCondition(t *testing.T) {
	conds := []metav1.Condition{
		{Type: odoov1.ConditionTypeCronReady, Status: condTrue, Reason: odoov1.ReasonCronRunning},
		{Type: odoov1.ConditionTypeReady, Status: condTrue, Reason: odoov1.ReasonAllReplicasReady},
	}

	// Disabled: CronReady is removed and Ready is left alone.
	applyCronCondition(&conds, 1, cronOutcome{})
	if meta.FindStatusCondition(conds, odoov1.ConditionTypeCronReady) != nil {
		t.Errorf("CronReady must be removed when cron is disabled")
	}
	if meta.FindStatusCondition(conds, odoov1.ConditionTypeReady) == nil {
		t.Errorf("Ready must be kept when cron is disabled")
	}

	// Enabled: CronReady is set from the outcome.
	applyCronCondition(&conds, 2, cronOutcome{enabled: true, deployFound: true, replicas: 1, ready: 1})
	c := meta.FindStatusCondition(conds, odoov1.ConditionTypeCronReady)
	if c == nil || c.Status != condTrue || c.ObservedGeneration != 2 {
		t.Errorf("CronReady = %+v, want True at generation 2", c)
	}
}

func TestRenderOdooConfCron(t *testing.T) {
	tests := []struct {
		name    string
		spec    odoov1.OdooInstanceSpec
		wantHas string
		wantNot string
		wantErr string
	}{
		{
			name:    "enabled forces max_cron_threads to 0",
			spec:    odoov1.OdooInstanceSpec{Cron: cronTestSpec()},
			wantHas: cronTestCfgLine,
		},
		{
			name:    "enabled rejects a user max_cron_threads",
			spec:    odoov1.OdooInstanceSpec{Cron: cronTestSpec(), Config: map[string]string{cronTestMaxKey: "4"}},
			wantErr: "cannot be set in spec.config",
		},
		{
			name:    "disabled emits no override",
			spec:    odoov1.OdooInstanceSpec{Cron: &odoov1.CronSpec{Enabled: false}},
			wantNot: cronTestMaxKey,
		},
		{
			name:    "absent cron emits no override",
			spec:    odoov1.OdooInstanceSpec{},
			wantNot: cronTestMaxKey,
		},
		{
			name:    "disabled keeps a user max_cron_threads",
			spec:    odoov1.OdooInstanceSpec{Cron: &odoov1.CronSpec{Enabled: false}, Config: map[string]string{cronTestMaxKey: "4"}},
			wantHas: "max_cron_threads = 4\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := renderOdooConf(&tc.spec)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("renderOdooConf() error = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("renderOdooConf() unexpected error: %v", err)
			}
			if tc.wantHas != "" && !strings.Contains(got, tc.wantHas) {
				t.Errorf("odoo.conf lacks %q:\n%s", tc.wantHas, got)
			}
			if tc.wantNot != "" && strings.Contains(got, tc.wantNot) {
				t.Errorf("odoo.conf must not contain %q:\n%s", tc.wantNot, got)
			}
		})
	}
}

// containsAll reports whether every want value is present in got.
func containsAll(got []string, want ...string) bool {
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
