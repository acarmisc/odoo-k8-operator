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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	tuNamespace     = "odoo"
	tuInstanceName  = "prod"
	tuUpgradeName   = "upg-1"
	tuDatabase      = "odoo_prod"
	tuImage         = "odoo:19.0-custom"
	tuNodeName      = "node-a"
	tuPasswordSec   = "pg-secret"
	tuModuleSale    = "sale"
	tuModuleStock   = "stock"
	tuModuleAccount = "account"
	tuAllModules    = "all"
	tuModulesJoined = "sale,stock"
	tuInstallJoined = "account"
	tuLongSuffix    = "-long-name-padding-to-exceed-the-kubernetes-label-limit"
	tuTrimmedLen    = 63
	tuRWO           = "ReadWriteOnce"
	tuRWX           = "ReadWriteMany"
	tuTimeoutSecs   = int64(120)
	tuOtherDB       = "other_db"
)

func tuUpgrade(name string, modules []string) *odoov1.OdooUpgrade {
	return &odoov1.OdooUpgrade{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tuNamespace},
		Spec: odoov1.OdooUpgradeSpec{
			InstanceRef: odoov1.OdooInstanceRef{Name: tuInstanceName},
			Modules:     modules,
		},
	}
}

func tuInstance(mutate func(*odoov1.OdooInstanceSpec)) *odoov1.OdooInstance {
	inst := &odoov1.OdooInstance{
		ObjectMeta: metav1.ObjectMeta{Name: tuInstanceName, Namespace: tuNamespace},
		Spec: odoov1.OdooInstanceSpec{
			Postgres: odoov1.PostgresSpec{Database: tuDatabase, PasswordSecret: tuPasswordSec},
		},
	}
	if mutate != nil {
		mutate(&inst.Spec)
	}
	return inst
}

func TestUpgradeArgs(t *testing.T) {
	tests := []struct {
		name           string
		database       string
		modules        []string
		installModules []string
		want           []string
	}{
		{
			name:     "default database, no modules",
			database: tuDatabase,
			want:     []string{upgradeFlagConfig, odooConfPath, upgradeFlagDB, tuDatabase, upgradeFlagStop, upgradeFlagNoHTTP},
		},
		{
			name:     "update only",
			database: tuDatabase,
			modules:  []string{tuModuleSale, tuModuleStock},
			want:     []string{upgradeFlagConfig, odooConfPath, upgradeFlagDB, tuDatabase, upgradeFlagUpdate, tuModulesJoined, upgradeFlagStop, upgradeFlagNoHTTP},
		},
		{
			name:           "install only",
			database:       tuDatabase,
			installModules: []string{tuModuleAccount},
			want:           []string{upgradeFlagConfig, odooConfPath, upgradeFlagDB, tuDatabase, upgradeFlagInstall, tuInstallJoined, upgradeFlagStop, upgradeFlagNoHTTP},
		},
		{
			name:           "update and install",
			database:       tuDatabase,
			modules:        []string{tuModuleSale, tuModuleStock},
			installModules: []string{tuModuleAccount},
			want: []string{upgradeFlagConfig, odooConfPath, upgradeFlagDB, tuDatabase,
				upgradeFlagUpdate, tuModulesJoined, upgradeFlagInstall, tuInstallJoined, upgradeFlagStop, upgradeFlagNoHTTP},
		},
		{
			name:     "update all",
			database: tuDatabase,
			modules:  []string{tuAllModules},
			want:     []string{upgradeFlagConfig, odooConfPath, upgradeFlagDB, tuDatabase, upgradeFlagUpdate, tuAllModules, upgradeFlagStop, upgradeFlagNoHTTP},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upgradeArgs(tt.database, tt.modules, tt.installModules)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("upgradeArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUpgradeJobName(t *testing.T) {
	long := strings.Repeat("a", 60) + tuLongSuffix
	other := strings.Repeat("a", 60) + "-long-name-padding-but-different"

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "short", in: tuUpgradeName, want: tuUpgradeName + "-run"},
		{name: "exactly at limit", in: strings.Repeat("b", 63-4), want: strings.Repeat("b", 59) + "-run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := upgradeJobName(tt.in); got != tt.want {
				t.Errorf("upgradeJobName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}

	t.Run("long name is truncated with hash and keeps suffix", func(t *testing.T) {
		got := upgradeJobName(long)
		if len(got) > tuTrimmedLen {
			t.Fatalf("len(%q) = %d, want <= %d", got, len(got), tuTrimmedLen)
		}
		if !strings.HasSuffix(got, "-run") {
			t.Errorf("name %q lost the -run suffix", got)
		}
		if got != upgradeJobName(long) {
			t.Errorf("truncated name is not stable")
		}
		if got == upgradeJobName(other) {
			t.Errorf("different long names collapsed to %q", got)
		}
	})

	t.Run("label value is bounded", func(t *testing.T) {
		got := upgradeLabelValue(long)
		if len(got) > tuTrimmedLen {
			t.Errorf("label value %q longer than 63", got)
		}
		if upgradeLabelValue(tuUpgradeName) != tuUpgradeName {
			t.Errorf("short label value must equal the name")
		}
	})
}

func TestUpgradeDatabaseResolution(t *testing.T) {
	inst := tuInstance(nil)
	if got := upgradeDatabase(tuUpgrade(tuUpgradeName, nil), inst); got != tuDatabase {
		t.Errorf("default database = %q, want %q", got, tuDatabase)
	}
	up := tuUpgrade(tuUpgradeName, nil)
	up.Spec.Database = tuOtherDB
	if got := upgradeDatabase(up, inst); got != tuOtherDB {
		t.Errorf("explicit database = %q, want %s", got, tuOtherDB)
	}
	empty := tuInstance(func(s *odoov1.OdooInstanceSpec) { s.Postgres.Database = "" })
	if got := upgradeDatabase(tuUpgrade(tuUpgradeName, nil), empty); got != "" {
		t.Errorf("no database configured should yield empty, got %q", got)
	}
}

func TestUpgradeNodePinRules(t *testing.T) {
	tests := []struct {
		name   string
		addons corev1.PersistentVolumeAccessMode
		data   corev1.PersistentVolumeAccessMode
		want   bool
	}{
		{name: "defaults are RWO", want: true},
		{name: "both RWX", addons: tuRWX, data: tuRWX, want: false},
		{name: "addons RWX, data RWO", addons: tuRWX, data: tuRWO, want: true},
		{name: "addons RWO, data RWX", addons: tuRWO, data: tuRWX, want: true},
		{name: "both RWO", addons: tuRWO, data: tuRWO, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := tuInstance(func(s *odoov1.OdooInstanceSpec) {
				s.Addons.AccessMode = tt.addons
				s.Data.AccessMode = tt.data
			})
			if got := upgradeNeedsNodePin(&inst.Spec); got != tt.want {
				t.Errorf("upgradeNeedsNodePin() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBuildUpgradeJobHardening(t *testing.T) {
	up := tuUpgrade(tuUpgradeName, []string{tuModuleSale})
	inst := tuInstance(func(s *odoov1.OdooInstanceSpec) { s.Image = tuImage })
	job := buildUpgradeJob(up, inst, tuDatabase, "")

	if job.Name != "upg-1-run" || job.Namespace != tuNamespace {
		t.Fatalf("job identity = %s/%s", job.Namespace, job.Name)
	}
	if job.Labels[labelManagedBy] != managedByOperator || job.Labels[upgradeLabelKey] != tuUpgradeName {
		t.Errorf("job labels = %v", job.Labels)
	}
	assertNoOdooServiceLabels(t, job.Labels)
	assertNoOdooServiceLabels(t, job.Spec.Template.Labels)
	assertUpgradeJobSpec(t, &job.Spec)
	assertUpgradePodSpec(t, &job.Spec.Template.Spec, tuImage)
	if !reflect.DeepEqual(job.Spec.Template.Spec.Containers[0].Args, upgradeArgs(tuDatabase, []string{tuModuleSale}, nil)) {
		t.Errorf("args = %q", job.Spec.Template.Spec.Containers[0].Args)
	}
	if job.Spec.Template.Spec.NodeSelector != nil {
		t.Errorf("no node pin expected without a node name, got %v", job.Spec.Template.Spec.NodeSelector)
	}
}

func assertUpgradeJobSpec(t *testing.T, spec *batchv1.JobSpec) {
	t.Helper()
	if spec.BackoffLimit == nil || *spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0", spec.BackoffLimit)
	}
	if spec.ActiveDeadlineSeconds == nil || *spec.ActiveDeadlineSeconds != upgradeDefaultTimeout {
		t.Errorf("activeDeadlineSeconds = %v, want %d", spec.ActiveDeadlineSeconds, upgradeDefaultTimeout)
	}
	if spec.TTLSecondsAfterFinished == nil || *spec.TTLSecondsAfterFinished != 86400 {
		t.Errorf("ttlSecondsAfterFinished = %v, want 86400", spec.TTLSecondsAfterFinished)
	}
}

func assertUpgradePodSpec(t *testing.T, pod *corev1.PodSpec, image string) {
	t.Helper()
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %s", pod.RestartPolicy)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Errorf("automountServiceAccountToken must be false")
	}
	psc := pod.SecurityContext
	if psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot ||
		*psc.RunAsUser != odooUID || *psc.RunAsGroup != odooUID || *psc.FSGroup != odooUID {
		t.Errorf("pod security context = %+v", psc)
	}
	if psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("seccomp profile = %+v", psc.SeccompProfile)
	}
	if len(pod.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Containers))
	}
	c := pod.Containers[0]
	if c.Image != image {
		t.Errorf("image = %q, want %q", c.Image, image)
	}
	assertUpgradeContainerSecurity(t, c.SecurityContext)
	assertUpgradeVolumes(t, pod.Volumes, c.VolumeMounts)
	assertUpgradeEnv(t, c.Env)
}

func assertUpgradeContainerSecurity(t *testing.T, sc *corev1.SecurityContext) {
	t.Helper()
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("allowPrivilegeEscalation must be false")
		return
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Errorf("readOnlyRootFilesystem must be true")
	}
	if sc.Capabilities == nil || !reflect.DeepEqual(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) {
		t.Errorf("capabilities drop = %+v", sc.Capabilities)
	}
}

func assertNoOdooServiceLabels(t *testing.T, labels map[string]string) {
	t.Helper()
	if labels[labelApp] == appOdoo || labels[labelInstance] != "" {
		t.Errorf("labels %v would be selected by the Odoo Service", labels)
	}
}

func assertUpgradeVolumes(t *testing.T, volumes []corev1.Volume, mounts []corev1.VolumeMount) {
	t.Helper()
	byName := map[string]corev1.Volume{}
	for _, v := range volumes {
		byName[v.Name] = v
	}
	addons := byName[volumeAddons].PersistentVolumeClaim
	if addons == nil || addons.ClaimName != addonsPVCName(tuInstanceName) || !addons.ReadOnly {
		t.Errorf("addons volume must be a read-only claim of the addons PVC: %+v", byName[volumeAddons])
	}
	data := byName[volumeData].PersistentVolumeClaim
	if data == nil || data.ClaimName != dataPVCName(tuInstanceName) || data.ReadOnly {
		t.Errorf("data volume must be a writable claim of the data PVC: %+v", byName[volumeData])
	}
	cfg := byName[volumeConfig].ConfigMap
	if cfg == nil || cfg.Name != configMapName(tuInstanceName) {
		t.Errorf("config volume = %+v", byName[volumeConfig])
	}
	if byName[volumeTmp].EmptyDir == nil {
		t.Errorf("tmp must be an emptyDir")
	}
	want := map[string]bool{
		mountPathConfig: true,
		addonsMountPath: true,
		mountPathData:   false,
		mountPathTmp:    false,
	}
	for _, m := range mounts {
		ro, ok := want[m.MountPath]
		if !ok {
			t.Errorf("unexpected mount %+v", m)
			continue
		}
		if m.ReadOnly != ro {
			t.Errorf("mount %s readOnly = %v, want %v", m.MountPath, m.ReadOnly, ro)
		}
		delete(want, m.MountPath)
	}
	if len(want) != 0 {
		t.Errorf("missing mounts: %v", want)
	}
}

func assertUpgradeEnv(t *testing.T, env []corev1.EnvVar) {
	t.Helper()
	found := map[string]bool{}
	for _, e := range env {
		found[e.Name] = true
		if e.Name == envPGPassword && (e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil ||
			e.ValueFrom.SecretKeyRef.Name != tuPasswordSec) {
			t.Errorf("PGPASSWORD must come from the password secret: %+v", e)
		}
		if e.Name == envPGSSLMode && e.Value != sslModeRequire {
			t.Errorf("PGSSLMODE = %q", e.Value)
		}
	}
	for _, name := range []string{envPGPassword, envDBPassword, envPGSSLMode} {
		if !found[name] {
			t.Errorf("env %s missing", name)
		}
	}
	if found[envAdminPasswd] {
		t.Errorf("the upgrade must not receive the admin password")
	}
}

func TestBuildUpgradeJobDefaultsAndPinning(t *testing.T) {
	up := tuUpgrade(tuUpgradeName, []string{tuAllModules})
	up.Spec.TimeoutSeconds = upgradePtr(tuTimeoutSecs)
	inst := tuInstance(nil)

	job := buildUpgradeJob(up, inst, tuDatabase, tuNodeName)
	if *job.Spec.ActiveDeadlineSeconds != tuTimeoutSecs {
		t.Errorf("activeDeadlineSeconds = %d, want %d", *job.Spec.ActiveDeadlineSeconds, tuTimeoutSecs)
	}
	if got := job.Spec.Template.Spec.NodeSelector[upgradeNodeHostLabel]; got != tuNodeName {
		t.Errorf("RWO volumes must pin to %s, got %q", tuNodeName, got)
	}
	if image := job.Spec.Template.Spec.Containers[0].Image; image != defaultOdooImage {
		t.Errorf("image = %q, want default %q", image, defaultOdooImage)
	}

	rwx := tuInstance(func(s *odoov1.OdooInstanceSpec) {
		s.Addons.AccessMode = tuRWX
		s.Data.AccessMode = tuRWX
	})
	free := buildUpgradeJob(up, rwx, tuDatabase, tuNodeName)
	if free.Spec.Template.Spec.NodeSelector != nil {
		t.Errorf("RWX volumes must not pin, got %v", free.Spec.Template.Spec.NodeSelector)
	}
}

func TestBuildUpgradeJobLongNameFits(t *testing.T) {
	up := tuUpgrade(strings.Repeat("x", 80), []string{tuModuleSale})
	job := buildUpgradeJob(up, tuInstance(nil), tuDatabase, "")
	if len(job.Name) > tuTrimmedLen {
		t.Errorf("job name %q longer than 63", job.Name)
	}
	if len(job.Labels[upgradeLabelKey]) > tuTrimmedLen {
		t.Errorf("label value longer than 63")
	}
}

func TestUpgradeTail(t *testing.T) {
	short := "hello\nworld"
	if got := upgradeTail(short, 2000); got != short {
		t.Errorf("short tail = %q", got)
	}
	long := strings.Repeat("a", 3000) + "\nlast line"
	got := upgradeTail(long, 20)
	if strings.Contains(got, "aaaa") || !strings.HasSuffix(got, "last line") {
		t.Errorf("tail = %q", got)
	}
	if len(got) > 20 {
		t.Errorf("tail longer than requested: %d", len(got))
	}
}
