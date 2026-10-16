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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	testAddonName    = "geox-theme"
	testAddonNS      = "odoo"
	testModule       = "geox_theme"
	testInstanceName = "prod"
	testSecretName   = "git-creds"
	testBranchRef    = "main"
	testSHARef       = "0123456789abcdef0123456789abcdef01234567"
	testHTTPSURL     = "https://github.com/acme/odoo-addons.git"
	testSSHURL       = "git@github.com:acme/odoo-addons.git"
	testSSHSchemeURL = "ssh://git@github.com/acme/odoo-addons.git"
	testAddonPath    = "addons"
	testPVCName      = "prod-addons"
	testNodeName     = "node-a"
	testSyncImage    = "alpine/git:test"
	testSecretPass   = "hunter2-do-not-log"
	testRootlessUID  = int64(65532)
	testRelRootLine  = "RELROOT=/mnt/odoo/addons/.releases/geox-theme"
	testRevParseLast = "git -C /tmp/repo rev-parse HEAD\n"
	testHTTPSAuthHdr = `http.extraHeader=Authorization: Basic $AUTH`
)

// syncScriptCase is one generated-script scenario: the spec plus fragments that must
// (or must not) appear in the script.
type syncScriptCase struct {
	name        string
	spec        syncSpec
	contains    []string
	notContains []string
}

func testSpec(mutate func(*syncSpec)) syncSpec {
	s := syncSpec{
		GitURL:     testHTTPSURL,
		GitRef:     testBranchRef,
		AddonName:  testAddonName,
		ModuleName: testModule,
		Auth:       authNone,
	}
	if mutate != nil {
		mutate(&s)
	}
	return s
}

func syncScriptCases() []syncScriptCase {
	return []syncScriptCase{
		{
			name: "single addon, branch, https token",
			spec: testSpec(func(s *syncSpec) {
				s.SingleAddon = true
				s.Auth = authHTTPSToken
				s.SecretName = testSecretName
			}),
			contains: []string{
				`git -c "http.extraHeader=Authorization: Basic $AUTH" clone --depth 1 --branch "$GIT_REF" "$GIT_URL" /tmp/repo`,
				testHTTPSAuthHdr,
				`GIT_USERNAME="${GIT_USERNAME:-x-access-token}"`,
				`AUTH=$(printf '%s:%s' "$GIT_USERNAME" "$GIT_TOKEN" | base64 | tr -d '\n')`,
				"publish geox_theme /tmp/repo\n",
				`cp -a "$src" "$REL/.$name.partial"`,
				`if [ ! -d "$REL/$name" ]; then`,
			},
			notContains: []string{"git init", "FETCH_HEAD", "__manifest__.py", "GIT_SSH_COMMAND"},
		},
		{
			name: "single addon, commit SHA, https token",
			spec: testSpec(func(s *syncSpec) {
				s.SingleAddon = true
				s.GitRef = testSHARef
				s.Auth = authHTTPSToken
				s.SecretName = testSecretName
			}),
			contains: []string{
				"git init /tmp/repo\n",
				`git -c "http.extraHeader=Authorization: Basic $AUTH" -C /tmp/repo fetch --depth 1 origin "$GIT_REF"`,
				"git -C /tmp/repo checkout --detach FETCH_HEAD",
				"publish geox_theme /tmp/repo\n",
			},
			notContains: []string{"--branch", "git clone"},
		},
		{
			name: "multi addon, branch, no secret, addonPath",
			spec: testSpec(func(s *syncSpec) {
				s.SingleAddon = false
				s.AddonPath = testAddonPath
			}),
			contains: []string{
				`git clone --depth 1 --branch "$GIT_REF" "$GIT_URL" /tmp/repo`,
				"for mod in /tmp/repo/addons/*/; do",
				`if [ -f "$mod/__manifest__.py" ]; then`,
				`publish "$(basename "$mod")" "$mod"`,
				`[ "$found" = 1 ] || { echo "no Odoo modules found in /tmp/repo/addons" >&2; exit 1; }`,
			},
			notContains: []string{testHTTPSAuthHdr, "Basic ", envGitToken, "GIT_SSH", "git init"},
		},
		{
			name: "multi addon, commit SHA, ssh secret",
			spec: testSpec(func(s *syncSpec) {
				s.GitURL = testSSHURL
				s.GitRef = testSHARef
				s.SingleAddon = false
				s.Auth = authSSH
				s.SecretName = testSecretName
			}),
			contains: []string{
				"git init /tmp/repo\n",
				`git -C /tmp/repo fetch --depth 1 origin "$GIT_REF"`,
				"for mod in /tmp/repo/*/; do",
			},
			notContains: []string{testHTTPSAuthHdr, "Basic ", "GIT_TOKEN", "--branch"},
		},
		{
			name: "single addon, branch, ssh secret",
			spec: testSpec(func(s *syncSpec) {
				s.GitURL = testSSHSchemeURL
				s.SingleAddon = true
				s.Auth = authSSH
				s.SecretName = testSecretName
			}),
			contains:    []string{`git clone --depth 1 --branch "$GIT_REF" "$GIT_URL" /tmp/repo`},
			notContains: []string{testHTTPSAuthHdr, "GIT_TOKEN", "ssh -i"},
		},
	}
}

// assertScriptInvariants checks properties every generated script must have, whatever the mode.
func assertScriptInvariants(t *testing.T, script string, s syncSpec) {
	t.Helper()
	if !strings.HasPrefix(script, "set -e\n") {
		t.Errorf("script must start with set -e")
	}
	if !strings.Contains(script, "set +x\n") {
		t.Errorf("script must disable shell tracing so credentials are never echoed")
	}
	if !strings.HasSuffix(script, testRevParseLast) {
		t.Errorf("last script line must be the commit SHA print, got tail %q", tail(script))
	}
	for _, want := range []string{testRelRootLine, `ln -sfn "$REL/$name" "$ADDONS/.$name.tmp"`, `mv -T "$ADDONS/.$name.tmp" "$target"`, "touch \"$REL\""} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing atomic-switch fragment %q", want)
		}
	}
	if !strings.Contains(script, "tail -n +3") {
		t.Errorf("script must prune to the two newest releases")
	}
	// Every line that writes to stdout must come before the final rev-parse; only the
	// rev-parse may print a bare value. Anything echoed must go to stderr.
	for _, line := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "echo ") && !strings.Contains(trimmed, ">&2") {
			t.Errorf("echo must go to stderr so stdout's last line stays the SHA: %q", trimmed)
		}
	}
	// The token value must never be inlined: it is only referenced via the env var.
	if strings.Contains(script, testSecretPass) {
		t.Errorf("script leaks a secret literal")
	}
	if s.Auth != authHTTPSToken && strings.Contains(script, "AUTH") {
		t.Errorf("non-token script must not reference AUTH")
	}
}

func tail(s string) string {
	if len(s) > 80 {
		return s[len(s)-80:]
	}
	return s
}

func TestBuildSyncScriptModes(t *testing.T) {
	for _, tc := range syncScriptCases() {
		t.Run(tc.name, func(t *testing.T) {
			script := buildSyncScript(tc.spec)
			assertScriptInvariants(t, script, tc.spec)
			for _, want := range tc.contains {
				if !strings.Contains(script, want) {
					t.Errorf("script missing %q\n--- script ---\n%s", want, script)
				}
			}
			for _, bad := range tc.notContains {
				if strings.Contains(script, bad) {
					t.Errorf("script must not contain %q\n--- script ---\n%s", bad, script)
				}
			}
		})
	}
}

func TestBuildSyncScriptSyntax(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	for _, tc := range syncScriptCases() {
		t.Run(tc.name, func(t *testing.T) {
			script := buildSyncScript(tc.spec)
			out, err := exec.Command(sh, "-n", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("sh -n rejected script: %v\n%s", err, out)
			}
		})
	}
}

func TestValidateGitUrl(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "https plain", url: testHTTPSURL},
		{name: "http plain", url: "http://git.internal/acme/addons.git"},
		{name: "scp style ssh", url: testSSHURL},
		{name: "ssh scheme with user only", url: testSSHSchemeURL},
		{name: "https with user and password", url: "https://user:" + testSecretPass + "@github.com/acme/x.git", wantErr: true},
		{name: "https with token as username", url: "https://ghp_abc@github.com/acme/x.git", wantErr: true},
		{name: "ssh scheme with password", url: "ssh://git:" + testSecretPass + "@github.com/acme/x.git", wantErr: true},
		{name: "unsupported scheme", url: "ftp://github.com/acme/x.git", wantErr: true},
		{name: "unparsable https", url: "https://[::1", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGitUrl(tc.url)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateGitUrl(%q) err = %v, wantErr %v", tc.url, err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), testSecretPass) {
				t.Errorf("validation error echoes the password: %v", err)
			}
		})
	}
}

func TestResolveSyncSpec(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*odoov1.OdooAddon)
		wantErr  bool
		wantRef  string
		wantMod  string
		wantAuth gitAuthMode
	}{
		{
			name:    "defaults ref and module name",
			mutate:  nil,
			wantRef: testBranchRef,
			wantMod: testAddonName,
		},
		{
			name: "https token secret",
			mutate: func(a *odoov1.OdooAddon) {
				a.Spec.GitSecretRef = &odoov1.LocalSecretRef{Name: testSecretName}
			},
			wantRef:  testBranchRef,
			wantMod:  testAddonName,
			wantAuth: authHTTPSToken,
		},
		{
			name: "ssh secret",
			mutate: func(a *odoov1.OdooAddon) {
				a.Spec.GitUrl = testSSHURL
				a.Spec.GitSecretRef = &odoov1.LocalSecretRef{Name: testSecretName}
			},
			wantRef:  testBranchRef,
			wantMod:  testAddonName,
			wantAuth: authSSH,
		},
		{
			name: "plain http with secret is refused",
			mutate: func(a *odoov1.OdooAddon) {
				a.Spec.GitUrl = "http://git.internal/acme/x.git"
				a.Spec.GitSecretRef = &odoov1.LocalSecretRef{Name: testSecretName}
			},
			wantErr: true,
		},
		{
			name: "empty secret name",
			mutate: func(a *odoov1.OdooAddon) {
				a.Spec.GitSecretRef = &odoov1.LocalSecretRef{}
			},
			wantErr: true,
		},
		{
			name:    "userinfo in url is rejected",
			mutate:  func(a *odoov1.OdooAddon) { a.Spec.GitUrl = "https://user:pass@github.com/acme/x.git" },
			wantErr: true,
		},
		{
			name:    "gitRef traversal is rejected",
			mutate:  func(a *odoov1.OdooAddon) { a.Spec.GitRef = "../main" },
			wantErr: true,
		},
		{
			name:    "module name cannot be a dot directory",
			mutate:  func(a *odoov1.OdooAddon) { a.Spec.ModuleName = ".releases" },
			wantErr: true,
		},
		{
			name:    "addonPath traversal is rejected",
			mutate:  func(a *odoov1.OdooAddon) { a.Spec.AddonPath = "../etc" },
			wantErr: true,
		},
		{
			name:    "commit SHA ref is accepted",
			mutate:  func(a *odoov1.OdooAddon) { a.Spec.GitRef = testSHARef },
			wantRef: testSHARef,
			wantMod: testAddonName,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addon := &odoov1.OdooAddon{
				ObjectMeta: metav1.ObjectMeta{Name: testAddonName, Namespace: testAddonNS},
				Spec: odoov1.OdooAddonSpec{
					GitUrl:      testHTTPSURL,
					InstanceRef: odoov1.OdooInstanceRef{Name: testInstanceName},
				},
			}
			if tc.mutate != nil {
				tc.mutate(addon)
			}
			got, err := resolveSyncSpec(addon)
			if (err != nil) != tc.wantErr {
				t.Fatalf("resolveSyncSpec err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got.GitRef != tc.wantRef {
				t.Errorf("GitRef = %q, want %q", got.GitRef, tc.wantRef)
			}
			if got.ModuleName != tc.wantMod {
				t.Errorf("ModuleName = %q, want %q", got.ModuleName, tc.wantMod)
			}
			if got.Auth != tc.wantAuth {
				t.Errorf("Auth = %v, want %v", got.Auth, tc.wantAuth)
			}
		})
	}
}

func TestIsCommitSHA(t *testing.T) {
	tests := []struct {
		ref  string
		want bool
	}{
		{ref: testSHARef, want: true},
		{ref: "0123456789abcdef0123456789abcdef0123456", want: false}, // 39 chars
		{ref: "0123456789abcdef0123456789abcdef012345678", want: false},
		{ref: "0123456789ABCDEF0123456789ABCDEF01234567", want: false},
		{ref: "0123456789abcdef0123456789abcdef0123456g", want: false},
		{ref: testBranchRef, want: false},
		{ref: "v1.2.3", want: false},
		{ref: testSHARef + "\n", want: false},
		{ref: "", want: false},
	}
	for _, tc := range tests {
		if got := isCommitSHA(tc.ref); got != tc.want {
			t.Errorf("isCommitSHA(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
}

func TestCanSkipResync(t *testing.T) {
	tests := []struct {
		name   string
		ref    string
		phase  string
		cloned string
		want   bool
	}{
		{name: "pinned and already synced", ref: testSHARef, phase: phaseSynced, cloned: testSHARef, want: true},
		{name: "pinned but failed", ref: testSHARef, phase: phaseFailed, cloned: testSHARef, want: false},
		{name: "pinned but a different commit was synced", ref: testSHARef, phase: phaseSynced, cloned: "ffffffffffffffffffffffffffffffffffffffff", want: false},
		{name: "pinned but never synced", ref: testSHARef, phase: phasePending, want: false},
		{name: "branch keeps resyncing", ref: testBranchRef, phase: phaseSynced, cloned: testSHARef, want: false},
		{name: "empty ref defaults to branch", ref: "", phase: phaseSynced, cloned: testSHARef, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			addon := &odoov1.OdooAddon{Spec: odoov1.OdooAddonSpec{GitRef: tc.ref}}
			addon.Status.Phase = tc.phase
			addon.Status.ClonedCommit = tc.cloned
			if got := canSkipResync(addon); got != tc.want {
				t.Errorf("canSkipResync = %v, want %v", got, tc.want)
			}
		})
	}
}

func testJob(t *testing.T, s syncSpec, opts syncJobOptions) *corev1.PodSpec {
	t.Helper()
	addon := &odoov1.OdooAddon{ObjectMeta: metav1.ObjectMeta{Name: testAddonName, Namespace: testAddonNS}}
	job := buildSyncJob(addon, s, opts)
	return &job.Spec.Template.Spec
}

func findEnv(env []corev1.EnvVar, name string) (corev1.EnvVar, bool) {
	for _, e := range env {
		if e.Name == name {
			return e, true
		}
	}
	return corev1.EnvVar{}, false
}

func findVolume(vols []corev1.Volume, name string) (corev1.Volume, bool) {
	for _, v := range vols {
		if v.Name == name {
			return v, true
		}
	}
	return corev1.Volume{}, false
}

func TestBuildSyncJobPodHardening(t *testing.T) {
	pod := testJob(t, testSpec(nil), syncJobOptions{Image: testSyncImage, PVCName: testPVCName, AddonsAccessMode: corev1.ReadWriteOnce})

	sc := pod.SecurityContext
	if sc == nil {
		t.Fatal("pod securityContext is nil")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("runAsNonRoot must be true")
	}
	for name, v := range map[string]*int64{"runAsUser": sc.RunAsUser, "runAsGroup": sc.RunAsGroup, "fsGroup": sc.FSGroup} {
		if v == nil || *v != testRootlessUID {
			t.Errorf("%s = %v, want %d", name, v, testRootlessUID)
		}
	}
	if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("seccomp profile must be RuntimeDefault")
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be false")
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never", pod.RestartPolicy)
	}
}

func TestBuildSyncJobContainerHardening(t *testing.T) {
	spec := testSpec(nil)
	pod := testJob(t, spec, syncJobOptions{Image: testSyncImage, PVCName: testPVCName})
	c := pod.Containers[0]

	if c.Image != testSyncImage {
		t.Errorf("image = %q", c.Image)
	}
	csc := c.SecurityContext
	if csc == nil {
		t.Fatal("container securityContext is nil")
	}
	if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must be false")
	}
	if csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must be true")
	}
	if csc.Capabilities == nil || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities must drop ALL, got %+v", csc.Capabilities)
	}

	limits := []struct {
		name string
		got  resource.Quantity
		want string
	}{
		{"cpu request", c.Resources.Requests.Cpu().DeepCopy(), "50m"},
		{"memory request", c.Resources.Requests.Memory().DeepCopy(), "64Mi"},
		{"cpu limit", c.Resources.Limits.Cpu().DeepCopy(), "500m"},
		{"memory limit", c.Resources.Limits.Memory().DeepCopy(), "256Mi"},
	}
	for _, l := range limits {
		if l.got.Cmp(resource.MustParse(l.want)) != 0 {
			t.Errorf("%s = %s, want %s", l.name, l.got.String(), l.want)
		}
	}

	if home, ok := findEnv(c.Env, envHome); !ok || home.Value != scratchMount {
		t.Errorf("HOME must be %s, got %+v", scratchMount, home)
	}
	if _, ok := findVolume(pod.Volumes, volumeScratch); !ok {
		t.Error("missing emptyDir scratch volume")
	}
	foundScratch := false
	for _, m := range c.VolumeMounts {
		if m.MountPath == scratchMount && m.Name == volumeScratch {
			foundScratch = true
		}
	}
	if !foundScratch {
		t.Error("scratch volume must be mounted at /tmp")
	}
	if len(c.Command) != 3 || c.Command[0] != "sh" || c.Command[1] != "-c" || c.Command[2] != buildSyncScript(spec) {
		t.Errorf("command must run the generated script via sh -c")
	}
}

func TestBuildSyncJobDeadlines(t *testing.T) {
	addon := &odoov1.OdooAddon{ObjectMeta: metav1.ObjectMeta{Name: testAddonName, Namespace: testAddonNS}}
	job := buildSyncJob(addon, testSpec(nil), syncJobOptions{PVCName: testPVCName})
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 180 {
		t.Error("job activeDeadlineSeconds must be 180")
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 1 {
		t.Error("job backoffLimit must be 1")
	}
}

func checkHTTPSSecretWiring(t *testing.T, pod *corev1.PodSpec) {
	t.Helper()
	tok, ok := findEnv(pod.Containers[0].Env, envGitToken)
	if !ok {
		t.Fatal("GIT_TOKEN env missing")
	}
	if tok.Value != "" {
		t.Error("GIT_TOKEN must not carry a literal value")
	}
	ref := tok.ValueFrom.SecretKeyRef
	if ref == nil || ref.Name != testSecretName || ref.Key != gitSecretTokenKey {
		t.Errorf("GIT_TOKEN secretKeyRef = %+v", ref)
	}
	if ref != nil && ref.Optional != nil && *ref.Optional {
		t.Error("token key must be required")
	}
	user, ok := findEnv(pod.Containers[0].Env, envGitUsername)
	if !ok || user.ValueFrom.SecretKeyRef == nil || user.ValueFrom.SecretKeyRef.Optional == nil || !*user.ValueFrom.SecretKeyRef.Optional {
		t.Error("GIT_USERNAME must be an optional secretKeyRef")
	}
	if _, ok := findEnv(pod.Containers[0].Env, envGitSSHCommand); ok {
		t.Error("https mode must not set GIT_SSH_COMMAND")
	}
	if _, ok := findVolume(pod.Volumes, gitSecretVolume); ok {
		t.Error("https mode must not mount the git secret volume")
	}
}

func checkSSHSecretWiring(t *testing.T, pod *corev1.PodSpec) {
	t.Helper()
	vol, ok := findVolume(pod.Volumes, gitSecretVolume)
	if !ok || vol.Secret == nil {
		t.Fatal("git secret volume missing")
	}
	if vol.Secret.SecretName != testSecretName {
		t.Errorf("secret name = %q", vol.Secret.SecretName)
	}
	if vol.Secret.DefaultMode == nil || *vol.Secret.DefaultMode != 0o400 {
		t.Errorf("defaultMode = %v, want 0400", vol.Secret.DefaultMode)
	}
	mounted := false
	for _, m := range pod.Containers[0].VolumeMounts {
		if m.Name == gitSecretVolume && m.MountPath == gitSecretMountPath && m.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("secret must be mounted read-only at %s", gitSecretMountPath)
	}
	sshEnv, ok := findEnv(pod.Containers[0].Env, envGitSSHCommand)
	if !ok {
		t.Fatal("GIT_SSH_COMMAND missing")
	}
	for _, want := range []string{
		"ssh -i " + gitSecretMountPath + "/" + gitSecretSSHKeyFile,
		"UserKnownHostsFile=" + gitSecretMountPath + "/" + gitSecretKnownHosts,
		"StrictHostKeyChecking=yes",
		"IdentitiesOnly=yes",
	} {
		if !strings.Contains(sshEnv.Value, want) {
			t.Errorf("GIT_SSH_COMMAND missing %q: %s", want, sshEnv.Value)
		}
	}
	if _, ok := findEnv(pod.Containers[0].Env, envGitToken); ok {
		t.Error("ssh mode must not expose GIT_TOKEN")
	}
}

func checkNoSecretWiring(t *testing.T, pod *corev1.PodSpec) {
	t.Helper()
	if len(pod.Volumes) != 2 {
		t.Errorf("want only addons + scratch volumes, got %d", len(pod.Volumes))
	}
	for _, name := range []string{envGitToken, envGitUsername, envGitSSHCommand} {
		if _, ok := findEnv(pod.Containers[0].Env, name); ok {
			t.Errorf("env %s must not be set without a secret", name)
		}
	}
}

func TestBuildSyncJobSecretWiring(t *testing.T) {
	tests := []struct {
		name  string
		spec  syncSpec
		check func(t *testing.T, pod *corev1.PodSpec)
	}{
		{
			name: "https token comes from secretKeyRef, never a literal",
			spec: testSpec(func(s *syncSpec) {
				s.Auth = authHTTPSToken
				s.SecretName = testSecretName
			}),
			check: checkHTTPSSecretWiring,
		},
		{
			name: "ssh key mounted read-only with restrictive mode",
			spec: testSpec(func(s *syncSpec) {
				s.GitURL = testSSHURL
				s.Auth = authSSH
				s.SecretName = testSecretName
			}),
			check: checkSSHSecretWiring,
		},
		{
			name:  "no secret means no credential wiring",
			spec:  testSpec(nil),
			check: checkNoSecretWiring,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, testJob(t, tc.spec, syncJobOptions{PVCName: testPVCName}))
		})
	}
}

func TestBuildSyncJobNodeSelector(t *testing.T) {
	tests := []struct {
		name    string
		node    string
		mode    corev1.PersistentVolumeAccessMode
		wantPin bool
	}{
		{name: "RWO pins to odoo pod node", node: testNodeName, mode: corev1.ReadWriteOnce, wantPin: true},
		{name: "unset access mode is treated as RWO", node: testNodeName, mode: "", wantPin: true},
		{name: "RWX does not pin", node: testNodeName, mode: corev1.ReadWriteMany, wantPin: false},
		{name: "no known node does not pin", node: "", mode: corev1.ReadWriteOnce, wantPin: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pod := testJob(t, testSpec(nil), syncJobOptions{PVCName: testPVCName, NodeName: tc.node, AddonsAccessMode: tc.mode})
			if tc.wantPin {
				if got := pod.NodeSelector["kubernetes.io/hostname"]; got != tc.node {
					t.Errorf("nodeSelector hostname = %q, want %q", got, tc.node)
				}
			} else if pod.NodeSelector != nil {
				t.Errorf("nodeSelector must be unset, got %v", pod.NodeSelector)
			}
		})
	}
}

func TestSyncImageEnvOverride(t *testing.T) {
	t.Setenv(syncImageEnvVar, "")
	if got := syncImage(); got != defaultSyncImage {
		t.Errorf("default syncImage = %q, want %q", got, defaultSyncImage)
	}
	t.Setenv(syncImageEnvVar, "registry.internal/git:9.9.9")
	if got := syncImage(); got != "registry.internal/git:9.9.9" {
		t.Errorf("env syncImage = %q", got)
	}
}
