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
	"bytes"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	tiModuleSale      = "sale"
	tiAdminSecret     = "odoo-admin-pw"
	tiEnvPasswordName = "ODOO_ADMIN_USER_PASSWORD"
	tiShellBinary     = "sh"
	tiSnippetRefLine  = "u = env.ref('base.user_admin')"
	tiSnippetPwLine   = "u.password = os.environ['ODOO_ADMIN_USER_PASSWORD']"
)

func tiInitUpgrade(installModules []string, adminSecret string) *odoov1.OdooUpgrade {
	up := tuUpgrade(tuUpgradeName, nil)
	up.Spec.InstallModules = installModules
	up.Spec.AdminUserPasswordSecret = adminSecret
	return up
}

// tiCat concatenates argument slices into a new slice.
func tiCat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// tiShSyntaxCheck runs `sh -n` on the script. It is skipped when no POSIX shell is installed.
func tiShSyntaxCheck(t *testing.T, script string) {
	t.Helper()
	shPath, err := exec.LookPath(tiShellBinary)
	if err != nil {
		t.Skipf("no %s binary: %v", tiShellBinary, err)
	}
	cmd := exec.Command(shPath, "-n")
	cmd.Stdin = strings.NewReader(script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("sh -n failed: %v: %s\nscript:\n%s", err, stderr.String(), script)
	}
}

func TestUpgradeArgsWithDemo(t *testing.T) {
	base := []string{upgradeFlagConfig, odooConfPath, upgradeFlagDB, tuDatabase}
	tail := []string{upgradeFlagStop, upgradeFlagNoHTTP}
	tests := []struct {
		name           string
		modules        []string
		installModules []string
		withDemo       bool
		want           []string
	}{
		{
			name:           "install without demo adds --without-demo=all",
			installModules: []string{tiEnvModule},
			want:           tiCat(base, []string{upgradeFlagInstall, tiEnvModule, upgradeFlagNoDemo}, tail),
		},
		{
			name:           "install with demo keeps demo data",
			installModules: []string{tiEnvModule},
			withDemo:       true,
			want:           tiCat(base, []string{upgradeFlagInstall, tiEnvModule}, tail),
		},
		{
			name:     "update only never adds the demo flag",
			modules:  []string{tiModuleSale},
			withDemo: false,
			want:     tiCat(base, []string{upgradeFlagUpdate, tiModuleSale}, tail),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := upgradeArgs(tuDatabase, tt.modules, tt.installModules, tt.withDemo)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("upgradeArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUpgradeInitScriptShape(t *testing.T) {
	args := upgradeArgs(tuDatabase, nil, []string{tiEnvModule}, false)
	script := upgradeInitScript(tuDatabase, args)

	lines := strings.Split(strings.TrimSpace(script), "\n")
	if lines[0] != "set -e" {
		t.Fatalf("first line = %q, want set -e", lines[0])
	}
	installLine := "odoo --config " + odooConfPath + " -d " + tuDatabase + " -i base --without-demo=all --stop-after-init --no-http"
	if lines[1] != installLine {
		t.Errorf("install line = %q, want %q", lines[1], installLine)
	}
	shellLine := "odoo shell --config " + odooConfPath + " -d " + tuDatabase + " --no-http <<'EOF'"
	if lines[2] != shellLine {
		t.Errorf("shell line = %q, want %q", lines[2], shellLine)
	}
	if !strings.Contains(script, tiSnippetRefLine) || !strings.Contains(script, tiSnippetPwLine) {
		t.Errorf("snippet missing from script:\n%s", script)
	}
	if !strings.Contains(script, "env.cr.commit()") {
		t.Errorf("snippet must commit:\n%s", script)
	}
	if !strings.HasSuffix(script, "\nEOF\n") {
		t.Errorf("heredoc is not terminated:\n%s", script)
	}
	tiShSyntaxCheck(t, script)
}

func TestUpgradeInitScriptNeverContainsPasswordValue(t *testing.T) {
	// The script is built without any password input: the only way the value reaches the
	// container is the environment. The script must reference the variable, not a literal.
	script := upgradeInitScript(tuDatabase, upgradeArgs(tuDatabase, nil, []string{tiEnvModule}, false))
	if !strings.Contains(script, "os.environ['"+tiEnvPasswordName+"']") {
		t.Errorf("script must read the password from %s", tiEnvPasswordName)
	}
	if strings.Contains(script, "--admin-passwd") || strings.Contains(script, "password=") {
		t.Errorf("script must not pass a password on the command line:\n%s", script)
	}
}

func TestUpgradeInitScriptQuotesUnsafeDatabaseName(t *testing.T) {
	db := "odoo'; rm -rf /"
	script := upgradeInitScript(db, upgradeArgs(db, nil, []string{tiEnvModule}, false))
	tiShSyntaxCheck(t, script)
	if strings.Contains(script, "-d "+db+" ") {
		t.Errorf("database name was not quoted:\n%s", script)
	}
}

func TestShQuoteWord(t *testing.T) {
	tests := map[string]string{
		"base":        "base",
		"a,b":         "a,b",
		"--no-http":   "--no-http",
		"":            "''",
		"it's":        `'it'\''s'`,
		"with space":  "'with space'",
		"$(id)":       "'$(id)'",
		"a;b":         "'a;b'",
		"ok/path.bin": "ok/path.bin",
	}
	for in, want := range tests {
		if got := shQuoteWord(in); got != want {
			t.Errorf("shQuoteWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUpgradeCommandSelection(t *testing.T) {
	plain := tiInitUpgrade([]string{tiEnvModule}, "")
	command, args := upgradeCommand(plain, tuDatabase)
	if command != nil {
		t.Errorf("no password: command = %q, want nil (image entrypoint)", command)
	}
	if !reflect.DeepEqual(args, upgradeArgs(tuDatabase, nil, []string{tiEnvModule}, false)) {
		t.Errorf("no password: args = %q", args)
	}

	withPw := tiInitUpgrade([]string{tiEnvModule}, tiAdminSecret)
	command, args = upgradeCommand(withPw, tuDatabase)
	if !reflect.DeepEqual(command, []string{upgradeShellPath, upgradeShellFlag}) {
		t.Errorf("password: command = %q", command)
	}
	if len(args) != 1 || args[0] != upgradeInitScript(tuDatabase, upgradeArgs(tuDatabase, nil, []string{tiEnvModule}, false)) {
		t.Errorf("password: args = %q", args)
	}
}

func TestBuildUpgradeJobInitEnvAndFlags(t *testing.T) {
	inst := tuInstance(func(s *odoov1.OdooInstanceSpec) { s.Image = tuImage })

	t.Run("password adds env from the Secret and the shell command", func(t *testing.T) {
		up := tiInitUpgrade([]string{tiEnvModule}, tiAdminSecret)
		job := buildUpgradeJob(up, inst, tuDatabase, "")
		c := job.Spec.Template.Spec.Containers[0]

		var found *corev1.EnvVar
		for i := range c.Env {
			if c.Env[i].Name == tiEnvPasswordName {
				found = &c.Env[i]
			}
		}
		if found == nil || found.ValueFrom == nil || found.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("env %s not injected from a Secret: %+v", tiEnvPasswordName, c.Env)
		}
		ref := found.ValueFrom.SecretKeyRef
		if ref.Name != tiAdminSecret || ref.Key != secretKeyPassword {
			t.Errorf("secret ref = %s/%s, want %s/%s", ref.Name, ref.Key, tiAdminSecret, secretKeyPassword)
		}
		if !reflect.DeepEqual(c.Command, []string{upgradeShellPath, upgradeShellFlag}) {
			t.Errorf("command = %q", c.Command)
		}
		if len(c.Args) != 1 {
			t.Fatalf("args = %q, want one script", c.Args)
		}
		for _, a := range c.Args {
			if strings.Contains(a, tiAdminSecret) {
				t.Errorf("secret name leaked into the script")
			}
		}
	})

	t.Run("no password keeps the entrypoint path and no password env", func(t *testing.T) {
		up := tiInitUpgrade([]string{tiEnvModule}, "")
		job := buildUpgradeJob(up, inst, tuDatabase, "")
		c := job.Spec.Template.Spec.Containers[0]
		if c.Command != nil {
			t.Errorf("command = %q, want nil", c.Command)
		}
		for _, e := range c.Env {
			if e.Name == tiEnvPasswordName {
				t.Errorf("unexpected env %s", tiEnvPasswordName)
			}
		}
		if !reflect.DeepEqual(c.Args, upgradeArgs(tuDatabase, nil, []string{tiEnvModule}, false)) {
			t.Errorf("args = %q", c.Args)
		}
	})
}

func TestUpgradeValidateSpecAdminPassword(t *testing.T) {
	tests := []struct {
		name    string
		modules []string
		install []string
		secret  string
		wantErr bool
	}{
		{name: "password with install", install: []string{tiEnvModule}, secret: tiAdminSecret},
		{name: "password without install", modules: []string{tiModuleSale}, secret: tiAdminSecret, wantErr: true},
		{name: "no password", modules: []string{tiModuleSale}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			up := tiInitUpgrade(tt.install, tt.secret)
			up.Spec.Modules = tt.modules
			err := upgradeValidateSpec(up)
			if (err != nil) != tt.wantErr {
				t.Errorf("upgradeValidateSpec() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
