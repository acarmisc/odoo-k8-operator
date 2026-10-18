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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	renderTestSrcConf = "[options]\nworkers = 2\n"
	renderTestPass    = "p%ss w0rd'\"$x"
	renderTestLine    = "admin_passwd = " + renderTestPass
	renderTestSizeMax = "1Mi"
)

// requireSh skips the test when there is no POSIX shell to run the render script with.
func requireSh(t *testing.T) string {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this host")
	}
	return sh
}

func adminInstance() *odoov1.OdooInstance {
	instance := &odoov1.OdooInstance{Spec: odoov1.OdooInstanceSpec{AdminPasswordSecret: testAdminSecret}}
	instance.Name = testInstanceName
	return instance
}

// writeRenderSource writes the ConfigMap copy the script reads and returns the source and
// destination paths inside a temporary directory.
func writeRenderSource(t *testing.T) (src, dst string) {
	t.Helper()
	dir := t.TempDir()
	src = filepath.Join(dir, "src.conf")
	dst = filepath.Join(dir, "rendered.conf")
	if err := os.WriteFile(src, []byte(renderTestSrcConf), 0o600); err != nil {
		t.Fatal(err)
	}
	return src, dst
}

// TestRenderConfigScriptRuns runs the script the way the init container does. The password
// must reach the rendered file and nothing else.
func TestRenderConfigScriptRuns(t *testing.T) {
	sh := requireSh(t)
	src, dst := writeRenderSource(t)

	cmd := exec.Command(sh, "-c", renderConfigScript(src, dst))
	cmd.Env = []string{"ODOO_ADMIN_PASSWD=" + renderTestPass}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render script failed: %v: %s", err, out)
	}
	if len(out) != 0 {
		t.Errorf("render script must be silent on success, got %q", out)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("rendered config missing: %v", err)
	}
	if want := renderTestSrcConf + renderTestLine + "\n"; string(got) != want {
		t.Errorf("rendered config = %q, want %q", got, want)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("rendered config mode = %o, want 600", perm)
	}
}

func TestRenderConfigScriptRejectsEmptyPassword(t *testing.T) {
	sh := requireSh(t)
	src, dst := writeRenderSource(t)

	cmd := exec.Command(sh, "-c", renderConfigScript(src, dst))
	cmd.Env = []string{"ODOO_ADMIN_PASSWD="}
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("render script must fail on an empty password")
	}
	if !strings.Contains(string(out), "ODOO_ADMIN_PASSWD is empty") {
		t.Errorf("failure message = %q, want the empty-password hint", out)
	}
	if _, statErr := os.Stat(dst); statErr == nil {
		t.Error("no rendered config may be written when the password is empty")
	}
}

func TestRenderConfigScriptSyntax(t *testing.T) {
	sh := requireSh(t)
	if out, err := exec.Command(sh, "-n", "-c", renderConfigScript(odooConfPath, renderedConfPath)).CombinedOutput(); err != nil {
		t.Fatalf("sh -n rejects the render script: %v: %s", err, out)
	}
}

func TestRenderConfigScriptNeverEchoesPassword(t *testing.T) {
	script := renderConfigScript(odooConfPath, renderedConfPath)
	if strings.Contains(script, "set -x") {
		t.Error("render script must not trace commands")
	}
	if strings.Contains(script, renderTestPass) {
		t.Error("render script must not contain the password")
	}
}

func TestBuildPodTemplateWithoutAdminPassword(t *testing.T) {
	instance := &odoov1.OdooInstance{}
	instance.Name = testInstanceName
	tmpl := buildPodTemplate(instance, rolloutHashes{})

	if len(tmpl.Spec.InitContainers) != 0 {
		t.Errorf("no init container without adminPasswordSecret, got %d", len(tmpl.Spec.InitContainers))
	}
	for _, v := range tmpl.Spec.Volumes {
		if v.Name == volumeRendered {
			t.Errorf("no %s volume without adminPasswordSecret", volumeRendered)
		}
	}
	if want := []string{upgradeFlagConfig, odooConfPath}; !reflect.DeepEqual(tmpl.Spec.Containers[0].Args, want) {
		t.Errorf("web args = %v, want %v", tmpl.Spec.Containers[0].Args, want)
	}
}

func TestBuildPodTemplateRendersAdminPassword(t *testing.T) {
	instance := adminInstance()
	tmpl := buildPodTemplate(instance, rolloutHashes{})
	container := tmpl.Spec.Containers[0]

	if want := []string{upgradeFlagConfig, renderedConfPath}; !reflect.DeepEqual(container.Args, want) {
		t.Errorf("web args = %v, want %v", container.Args, want)
	}
	if _, ok := envByName(container.Env, envAdminPasswd); ok {
		t.Errorf("web container must not receive %s", envAdminPasswd)
	}
	assertRenderedMount(t, container.VolumeMounts)
	assertRenderedVolume(t, tmpl.Spec.Volumes)

	if len(tmpl.Spec.InitContainers) != 1 {
		t.Fatalf("init containers = %d, want 1", len(tmpl.Spec.InitContainers))
	}
	init := tmpl.Spec.InitContainers[0]
	if init.Name != containerRenderConfig {
		t.Errorf("init container name = %q, want %s", init.Name, containerRenderConfig)
	}
	if init.Image != container.Image {
		t.Errorf("init image = %q, want the web image %q", init.Image, container.Image)
	}
	if !reflect.DeepEqual(init.SecurityContext, container.SecurityContext) {
		t.Errorf("init securityContext = %+v, want the web container posture %+v", init.SecurityContext, container.SecurityContext)
	}
	wantCommand := []string{upgradeShellPath, upgradeShellFlag, renderConfigScript(odooConfPath, renderedConfPath)}
	if !reflect.DeepEqual(init.Command, wantCommand) {
		t.Errorf("init command does not match the render script")
	}
	if got := init.Resources.Requests.Cpu().String(); got != renderRequestCPU {
		t.Errorf("init cpu request = %s, want %s", got, renderRequestCPU)
	}
	if got := init.Resources.Limits.Memory().String(); got != renderLimitMemory {
		t.Errorf("init memory limit = %s, want %s", got, renderLimitMemory)
	}
	assertInitMounts(t, init.VolumeMounts)
	assertInitAdminEnv(t, init.Env)
}

func assertRenderedMount(t *testing.T, mounts []corev1.VolumeMount) {
	t.Helper()
	for _, m := range mounts {
		if m.Name == volumeRendered {
			if m.MountPath != mountPathRendered || !m.ReadOnly {
				t.Errorf("rendered volume mount = %+v, want read-only at %s", m, mountPathRendered)
			}
			return
		}
	}
	t.Errorf("web container does not mount %s", volumeRendered)
}

func assertRenderedVolume(t *testing.T, volumes []corev1.Volume) {
	t.Helper()
	for _, v := range volumes {
		if v.Name != volumeRendered {
			continue
		}
		if v.EmptyDir == nil {
			t.Fatalf("%s must be an emptyDir", volumeRendered)
		}
		if v.EmptyDir.Medium != corev1.StorageMediumMemory {
			t.Errorf("%s medium = %q, want Memory", volumeRendered, v.EmptyDir.Medium)
		}
		if v.EmptyDir.SizeLimit == nil || v.EmptyDir.SizeLimit.Cmp(resource.MustParse(renderTestSizeMax)) != 0 {
			t.Errorf("%s sizeLimit = %v, want %s", volumeRendered, v.EmptyDir.SizeLimit, renderTestSizeMax)
		}
		return
	}
	t.Errorf("pod has no %s volume", volumeRendered)
}

func assertInitMounts(t *testing.T, mounts []corev1.VolumeMount) {
	t.Helper()
	byName := map[string]corev1.VolumeMount{}
	for _, m := range mounts {
		byName[m.Name] = m
	}
	if m := byName[volumeConfig]; m.MountPath != mountPathConfig || !m.ReadOnly {
		t.Errorf("init config mount = %+v, want read-only at %s", m, mountPathConfig)
	}
	if m := byName[volumeRendered]; m.MountPath != mountPathRendered || m.ReadOnly {
		t.Errorf("init rendered mount = %+v, want writable at %s", m, mountPathRendered)
	}
}

func assertInitAdminEnv(t *testing.T, env []corev1.EnvVar) {
	t.Helper()
	if len(env) != 1 {
		t.Fatalf("init container env = %v, want only %s", env, envAdminPasswd)
	}
	e, ok := envByName(env, envAdminPasswd)
	if !ok || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("init container must read %s from a Secret", envAdminPasswd)
	}
	ref := e.ValueFrom.SecretKeyRef
	if ref.Name != testAdminSecret || ref.Key != secretKeyPassword {
		t.Errorf("init secret ref = %s/%s, want %s/%s", ref.Name, ref.Key, testAdminSecret, secretKeyPassword)
	}
}

func TestAdminPasswordKeptOutOfCronPod(t *testing.T) {
	instance := adminInstance()
	instance.Spec.Cron = cronTestSpec()
	tmpl := buildCronPodTemplate(instance, rolloutHashes{})
	container := tmpl.Spec.Containers[0]

	if _, ok := envByName(container.Env, envAdminPasswd); ok {
		t.Errorf("cron container must not receive %s", envAdminPasswd)
	}
	if len(tmpl.Spec.InitContainers) != 0 {
		t.Error("cron pod must not render the config")
	}
	if want := []string{upgradeFlagConfig, odooConfPath}; !reflect.DeepEqual(container.Args[:2], want) {
		t.Errorf("cron args = %v, want the /etc/odoo config", container.Args)
	}
}
