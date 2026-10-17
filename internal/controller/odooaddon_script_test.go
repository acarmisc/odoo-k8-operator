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
	"os/exec"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

// inOrder fails the test unless every marker occurs in script, each after the previous one.
func inOrder(t *testing.T, script string, markers ...string) {
	t.Helper()
	last := -1
	for _, m := range markers {
		idx := strings.Index(script, m)
		if idx < 0 {
			t.Fatalf("script missing %q", m)
		}
		if idx < last {
			t.Errorf("%q must come after the previous step in the script", m)
		}
		last = idx
	}
}

// Conflicts must be found before any copy, link switch or directory creation, so a
// conflict leaves the addons volume exactly as it was.
func TestBuildSyncScriptConflictCheckPrecedesAnyWrite(t *testing.T) {
	for _, tc := range syncScriptCases() {
		t.Run(tc.name, func(t *testing.T) {
			inOrder(t, buildSyncScript(tc.spec),
				`: > "$MODLIST"`,
				`while IFS= read -r name; do`,
				`already provided by addon`,
				`mkdir -p "$REL"`,
				`cp -a "$src"`,
				`mv -T "$ADDONS/.$name.tmp" "$target"`,
			)
		})
	}
}

// A sync publishes first, removes stale links next, and only then records the new
// manifest and prunes old releases. The modules line is printed right before the SHA.
func TestBuildSyncScriptStepOrder(t *testing.T) {
	for _, tc := range syncScriptCases() {
		t.Run(tc.name, func(t *testing.T) {
			inOrder(t, buildSyncScript(tc.spec),
				`touch "$REL"`,
				`done < "$PREV"`,
				`mv "$RELROOT/.modules.tmp" "$RELROOT/.modules"`,
				`tail -n +3`,
				`printf 'modules: %s\n'`,
				testRevParseLast,
			)
		})
	}
}

func TestBuildSyncScriptManifestFragments(t *testing.T) {
	for _, tc := range syncScriptCases() {
		t.Run(tc.name, func(t *testing.T) {
			script := buildSyncScript(tc.spec)
			for _, want := range []string{
				`if [ -f "$RELROOT/.modules" ]; then cp "$RELROOT/.modules" "$PREV"; else : > "$PREV"; fi`,
				`cp "$MODLIST" "$RELROOT/.modules.tmp"`,
				`if grep -qxF "$old" "$MODLIST"; then continue; fi`,
				`"$RELROOT/"*) rm -f "$target" ;;`,
				`printf 'modules: %s\n' "$(paste -sd, "$MODLIST")"`,
			} {
				if !strings.Contains(script, want) {
					t.Errorf("script missing %q", want)
				}
			}
		})
	}
}

func TestBuildSyncScriptConflictMessage(t *testing.T) {
	script := buildSyncScript(testSpec(nil))
	want := `echo "module $name is already provided by addon $other" >&2`
	if !strings.Contains(script, want) {
		t.Errorf("conflict must fail with a clear stderr message, script missing %q", want)
	}
	if !strings.Contains(script, `"$ADDONS/.releases/"*)`) {
		t.Errorf("conflict check must only consider links into the release store")
	}
}

func TestBuildCleanupScript(t *testing.T) {
	script := buildCleanupScript(testAddonName)
	for _, want := range []string{
		testRelRootLine,
		`find "$ADDONS" -mindepth 1 -maxdepth 1 -type l`,
		`"$RELROOT/"*) rm -f "$link" ;;`,
		`rm -rf "$RELROOT"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("cleanup script missing %q\n%s", want, script)
		}
	}
	for _, bad := range []string{"git", "GIT_", `rm -rf "$ADDONS"`} {
		if strings.Contains(script, bad) {
			t.Errorf("cleanup script must not contain %q", bad)
		}
	}
	inOrder(t, script, `rm -f "$link"`, `rm -rf "$RELROOT"`)
	if !strings.HasPrefix(script, "set -e\n") || !strings.Contains(script, "set +x\n") {
		t.Errorf("cleanup script must fail fast and never trace")
	}
}

func TestBuildCleanupScriptSyntax(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	out, err := exec.Command(sh, "-n", "-c", buildCleanupScript(testAddonName)).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -n rejected cleanup script: %v\n%s", err, out)
	}
}

func TestBuildCleanupJobHardening(t *testing.T) {
	addon := &odoov1.OdooAddon{ObjectMeta: metav1.ObjectMeta{Name: testAddonName, Namespace: testAddonNS}}
	job := buildCleanupJob(addon, syncJobOptions{
		Image:            testSyncImage,
		PVCName:          testPVCName,
		NodeName:         testNodeName,
		AddonsAccessMode: corev1.ReadWriteOnce,
	})

	if job.Name != testAddonName+cleanupJobSuffix {
		t.Errorf("cleanup Job name = %q", job.Name)
	}
	if job.Namespace != testAddonNS {
		t.Errorf("cleanup Job namespace = %q", job.Namespace)
	}
	pod := &job.Spec.Template.Spec
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != testRootlessUID {
		t.Errorf("cleanup pod must run as uid %d", testRootlessUID)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Errorf("cleanup pod must not mount a service account token")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != syncActiveDeadlineSeconds {
		t.Errorf("cleanup Job must carry the active deadline")
	}
	if pod.Containers[0].SecurityContext == nil || !*pod.Containers[0].SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("cleanup container must have a read-only root filesystem")
	}
	if pod.NodeSelector["kubernetes.io/hostname"] != testNodeName {
		t.Errorf("RWO addons volume must pin the cleanup pod to the Odoo node")
	}
	if _, ok := findEnv(pod.Containers[0].Env, envGitToken); ok {
		t.Errorf("cleanup Job must not carry git credentials")
	}
	if got := pod.Containers[0].Command; len(got) != 3 || got[2] != buildCleanupScript(testAddonName) {
		t.Errorf("cleanup Job must run the cleanup script, got %v", got)
	}
}

func TestParseSyncOutput(t *testing.T) {
	tests := []struct {
		name    string
		logs    string
		commit  string
		modules []string
	}{
		{name: "modules line then sha", logs: "modules: geox_theme,sales\n" + testSHARef + "\n", commit: testSHARef, modules: []string{testModule, testAddonSales}},
		{name: "modules are sorted and de-duplicated", logs: "modules: sales,geox_theme,sales\n" + testSHARef, commit: testSHARef, modules: []string{testModule, testAddonSales}},
		{name: "no modules line", logs: testSHARef + "\n", commit: testSHARef},
		{name: "empty module list", logs: "modules: \n" + testSHARef, commit: testSHARef},
		{name: "empty entry in list", logs: "modules: a,,b\n" + testSHARef, commit: testSHARef},
		{name: "traversal in a name", logs: "modules: a,../b\n" + testSHARef, commit: testSHARef},
		{name: "dot-prefixed name", logs: "modules: .hidden\n" + testSHARef, commit: testSHARef},
		{name: "wrong prefix", logs: "module: a\n" + testSHARef, commit: testSHARef},
		{name: "noise before the modules line", logs: "warning: something\nmodules: a\n" + testSHARef + "\n", commit: testSHARef, modules: []string{"a"}},
		{name: "noise between modules and sha", logs: "modules: a\nnoise\n" + testSHARef, commit: testSHARef},
		{name: "blank lines are ignored", logs: "\n\nmodules: a\n\n" + testSHARef + "\n\n", commit: testSHARef, modules: []string{"a"}},
		{name: "no output at all", logs: "", commit: ""},
		{name: "whitespace only", logs: "\n  \n", commit: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			commit, modules := parseSyncOutput(tc.logs)
			if commit != tc.commit {
				t.Errorf("commit = %q, want %q", commit, tc.commit)
			}
			if strings.Join(modules, ",") != strings.Join(tc.modules, ",") {
				t.Errorf("modules = %v, want %v", modules, tc.modules)
			}
		})
	}
}
