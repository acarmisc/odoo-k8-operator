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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	testInstanceName = "unit-instance"
	testPGHost       = "db.internal"
	testPGDatabase   = "odoo"
	testPGUser       = "odoo_user"
	testPGPort       = 5432
	testLinePrefix   = "[options]"
	lineAddonsPath   = "addons_path = /mnt/odoo/addons"
	lineListDBFalse  = "list_db = False"
	lineProxyTrue    = "proxy_mode = True"
	testKeyWorkers   = "workers"
	testKeyExtra     = "extra"
	errNotPermitted  = "not permitted"
)

// defaultConfLines are the lines every rendered odoo.conf starts with when
// the spec does not override the defaults and has no database configured.
var defaultConfLines = []string{
	testLinePrefix,
	lineAddonsPath,
	lineListDBFalse,
	lineProxyTrue,
}

func joinConfLines(lines []string) string {
	return strings.Join(lines, "\n") + "\n"
}

func TestRenderOdooConf(t *testing.T) {
	tests := []struct {
		name    string
		spec    odoov1.OdooInstanceSpec
		want    []string
		wantErr string
	}{
		{
			name: "defaults only without a database",
			spec: odoov1.OdooInstanceSpec{},
			want: defaultConfLines,
		},
		{
			name: "user keys are emitted in sorted order",
			spec: odoov1.OdooInstanceSpec{Config: map[string]string{
				testKeyWorkers:      "4",
				"max_cron_threads":  "2",
				"limit_time_real":   "120",
				"unaccent":          "True",
				"data_dir_override": "/data",
			}},
			want: []string{
				testLinePrefix,
				lineAddonsPath,
				"data_dir_override = /data",
				"limit_time_real = 120",
				lineListDBFalse,
				"max_cron_threads = 2",
				lineProxyTrue,
				"unaccent = True",
				"workers = 4",
			},
		},
		{
			name: "user value overrides list_db default",
			spec: odoov1.OdooInstanceSpec{Config: map[string]string{"list_db": "True"}},
			want: []string{
				testLinePrefix,
				lineAddonsPath,
				"list_db = True",
				lineProxyTrue,
			},
		},
		{
			name: "user value overrides proxy_mode default",
			spec: odoov1.OdooInstanceSpec{Config: map[string]string{"proxy_mode": "False"}},
			want: []string{
				testLinePrefix,
				lineAddonsPath,
				lineListDBFalse,
				"proxy_mode = False",
			},
		},
		{
			name: "database settings are rendered with sslmode",
			spec: odoov1.OdooInstanceSpec{Postgres: odoov1.PostgresSpec{
				Host: testPGHost, Port: testPGPort, Database: testPGDatabase, User: testPGUser,
			}},
			want: []string{
				testLinePrefix,
				lineAddonsPath,
				"db_host = db.internal",
				"db_name = odoo",
				"db_port = 5432",
				"db_sslmode = require",
				"db_user = odoo_user",
				lineListDBFalse,
				lineProxyTrue,
			},
		},
		{
			name:    "admin_passwd is blocked",
			spec:    odoov1.OdooInstanceSpec{Config: map[string]string{"admin_passwd": "secret"}},
			wantErr: errNotPermitted,
		},
		{
			name:    "db_password is blocked",
			spec:    odoov1.OdooInstanceSpec{Config: map[string]string{"db_password": "secret"}},
			wantErr: errNotPermitted,
		},
		{
			name:    "dbfilter is blocked",
			spec:    odoov1.OdooInstanceSpec{Config: map[string]string{"dbfilter": ".*"}},
			wantErr: errNotPermitted,
		},
		{
			name:    "newline in a config value is rejected",
			spec:    odoov1.OdooInstanceSpec{Config: map[string]string{"workers": "4\nadmin_passwd = x"}},
			wantErr: "illegal newline",
		},
		{
			name:    "newline in a database value is rejected",
			spec:    odoov1.OdooInstanceSpec{Postgres: odoov1.PostgresSpec{Host: "db\nx", Port: testPGPort, Database: testPGDatabase}},
			wantErr: "illegal newline",
		},
		{
			name:    "key containing an equals sign is rejected",
			spec:    odoov1.OdooInstanceSpec{Config: map[string]string{"bad=key": "1"}},
			wantErr: "illegal characters",
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
			if want := joinConfLines(tc.want); got != want {
				t.Errorf("renderOdooConf() mismatch\n got:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestRenderOdooConfIsDeterministic(t *testing.T) {
	spec := &odoov1.OdooInstanceSpec{Config: map[string]string{
		"a": "1", "b": "2", "c": "3", "d": "4", "e": "5", "f": "6", "g": "7", "h": "8",
	}}
	first, err := renderOdooConf(spec)
	if err != nil {
		t.Fatalf("renderOdooConf() unexpected error: %v", err)
	}
	// Map iteration order is randomized per range; repeated renders must still agree.
	for i := 0; i < 50; i++ {
		got, renderErr := renderOdooConf(spec)
		if renderErr != nil {
			t.Fatalf("renderOdooConf() unexpected error: %v", renderErr)
		}
		if got != first {
			t.Fatalf("render %d differs from the first render:\n%s\nvs\n%s", i, got, first)
		}
	}
}

func TestSecretsHash(t *testing.T) {
	newSecret := func(data map[string][]byte) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
			Data:       data,
		}
	}
	base := newSecret(map[string][]byte{secretKeyPassword: []byte("one"), testKeyExtra: []byte("x")})
	sameDataDifferentInsertion := newSecret(map[string][]byte{testKeyExtra: []byte("x"), secretKeyPassword: []byte("one")})
	rotated := newSecret(map[string][]byte{secretKeyPassword: []byte("two"), testKeyExtra: []byte("x")})
	otherKey := newSecret(map[string][]byte{"other": []byte("one"), testKeyExtra: []byte("x")})
	admin := newSecret(map[string][]byte{secretKeyPassword: []byte("admin")})

	if got := secretsHash([]*corev1.Secret{base}); got != secretsHash([]*corev1.Secret{sameDataDifferentInsertion}) {
		t.Errorf("secretsHash should not depend on map insertion order")
	}
	if secretsHash([]*corev1.Secret{base}) == secretsHash([]*corev1.Secret{rotated}) {
		t.Errorf("secretsHash should change when a value is rotated")
	}
	if secretsHash([]*corev1.Secret{base}) == secretsHash([]*corev1.Secret{otherKey}) {
		t.Errorf("secretsHash should change when a key is renamed")
	}
	if secretsHash([]*corev1.Secret{base, admin}) == secretsHash([]*corev1.Secret{admin, base}) {
		t.Errorf("secretsHash should depend on the order secrets are passed")
	}
	if len(secretsHash(nil)) != 64 {
		t.Errorf("secretsHash of no secrets should still be a sha256 hex digest")
	}
}

func TestConfigHash(t *testing.T) {
	a := configHash("[options]\nworkers = 4\n")
	if a != configHash("[options]\nworkers = 4\n") {
		t.Errorf("configHash should be stable for identical input")
	}
	if a == configHash("[options]\nworkers = 5\n") {
		t.Errorf("configHash should change when the rendered config changes")
	}
	// Known sha256 vector for the empty string guards the algorithm choice.
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := sha256Hex(""); got != emptySHA256 {
		t.Errorf("sha256Hex(\"\") = %s, want %s", got, emptySHA256)
	}
}

func readyAddon(name, instance, commit string, ready bool) odoov1.OdooAddon {
	return odoov1.OdooAddon{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       odoov1.OdooAddonSpec{InstanceRef: odoov1.OdooInstanceRef{Name: instance}},
		Status:     odoov1.OdooAddonStatus{Ready: ready, ClonedCommit: commit},
	}
}

func TestFilterReadyAddons(t *testing.T) {
	all := []odoov1.OdooAddon{
		readyAddon("theme", testInstanceName, "aaa", true),
		readyAddon("pending", testInstanceName, "", false),
		readyAddon("elsewhere", "other-instance", "bbb", true),
		readyAddon(testAddonSales, testInstanceName, "ccc", true),
	}
	got := filterReadyAddons(all, testInstanceName)
	if len(got) != 2 || got[0].Name != "theme" || got[1].Name != testAddonSales {
		t.Fatalf("filterReadyAddons() = %v, want theme and sales", got)
	}
}

// Names shared by the addon path and hash tests.
const (
	testAddonSales = "sales"
	testModA       = "a_mod"
	testModB       = "b_mod"
)

func TestAddonRevisionsAndHash(t *testing.T) {
	forward := []odoov1.OdooAddon{
		readyAddon("theme", testInstanceName, "aaa", true),
		readyAddon(testAddonSales, testInstanceName, "ccc", true),
	}
	reversed := []odoov1.OdooAddon{forward[1], forward[0]}

	if got, want := addonRevisions(reversed), []string{"sales=ccc", "theme=aaa"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("addonRevisions() = %v, want %v", got, want)
	}
	if addonsHash(forward) != addonsHash(reversed) {
		t.Errorf("addonsHash should not depend on addon list order")
	}

	bumped := []odoov1.OdooAddon{
		readyAddon("theme", testInstanceName, "aaa2", true),
		readyAddon(testAddonSales, testInstanceName, "ccc", true),
	}
	if addonsHash(forward) == addonsHash(bumped) {
		t.Errorf("addonsHash should change when an addon is re-synced to a new commit")
	}
	if addonsHash(nil) != sha256Hex("") {
		t.Errorf("addonsHash of no addons should hash the empty revision list")
	}
}

func TestAddonMountPaths(t *testing.T) {
	singleModule := readyAddon("geox-theme", testInstanceName, "aaa", true)
	singleModule.Spec.ModuleName = testModule
	addons := []odoov1.OdooAddon{singleModule, readyAddon(testAddonSales, testInstanceName, "ccc", true)}

	got := addonMountPaths(addons)
	want := []string{"/mnt/odoo/addons/geox_theme", "/mnt/odoo/addons/" + testAddonSales}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("addonMountPaths() = %v, want %v", got, want)
	}
}

func TestAddonMountPathsUsesReportedModules(t *testing.T) {
	// A multi-module repo: the OdooAddon name and moduleName say nothing about the
	// module folders, so the paths must come from what the sync reported.
	multi := readyAddon("theme-pack", testInstanceName, "aaa", true)
	multi.Spec.ModuleName = "ignored_name"
	multi.Status.Modules = []string{"website_theme", "sale_theme"}
	legacy := readyAddon(testAddonSales, testInstanceName, "ccc", true)

	got := addonMountPaths([]odoov1.OdooAddon{multi, legacy})
	want := []string{"/mnt/odoo/addons/sale_theme", "/mnt/odoo/addons/" + testAddonSales, "/mnt/odoo/addons/website_theme"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("addonMountPaths() = %v, want %v", got, want)
	}
}

func TestAddonMountPathsFallsBackWithoutModules(t *testing.T) {
	noModuleName := readyAddon(testAddonSales, testInstanceName, "ccc", true)
	withModuleName := readyAddon("geox-theme", testInstanceName, "aaa", true)
	withModuleName.Spec.ModuleName = testModule

	got := addonMountPaths([]odoov1.OdooAddon{noModuleName, withModuleName})
	want := []string{"/mnt/odoo/addons/geox_theme", "/mnt/odoo/addons/" + testAddonSales}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("addonMountPaths() = %v, want %v", got, want)
	}
	if got := addonMountPaths(nil); len(got) != 0 {
		t.Errorf("addonMountPaths(nil) = %v, want none", got)
	}
}

func TestAddonsHashTracksReportedModules(t *testing.T) {
	plain := readyAddon("theme", testInstanceName, "aaa", true)
	if got, want := addonRevisions([]odoov1.OdooAddon{plain}), []string{"theme=aaa"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("addonRevisions() without modules = %v, want %v", got, want)
	}

	withModules := plain
	withModules.Status.Modules = []string{testModB, testModA}
	reordered := plain
	reordered.Status.Modules = []string{testModA, testModB}
	changed := plain
	changed.Status.Modules = []string{testModA}

	if addonsHash([]odoov1.OdooAddon{withModules}) != addonsHash([]odoov1.OdooAddon{reordered}) {
		t.Errorf("addonsHash must not depend on the order modules were reported in")
	}
	if addonsHash([]odoov1.OdooAddon{withModules}) == addonsHash([]odoov1.OdooAddon{plain}) {
		t.Errorf("addonsHash must change when modules appear")
	}
	if addonsHash([]odoov1.OdooAddon{withModules}) == addonsHash([]odoov1.OdooAddon{changed}) {
		t.Errorf("addonsHash must change when the module set changes")
	}
}

func TestOwnerRefHelpers(t *testing.T) {
	const keepUID types.UID = "keep"
	const dropUID types.UID = "drop"
	refs := []metav1.OwnerReference{
		{Name: "a", UID: keepUID},
		{Name: "b", UID: dropUID},
	}

	if !hasOwnerRef(refs, dropUID) {
		t.Errorf("hasOwnerRef should find the drop reference")
	}
	if hasOwnerRef(refs, "missing") {
		t.Errorf("hasOwnerRef should not match an unknown UID")
	}

	kept := ownerRefsWithoutUID(refs, dropUID)
	if len(kept) != 1 || kept[0].UID != keepUID {
		t.Errorf("ownerRefsWithoutUID() = %v, want only the keep reference", kept)
	}
	if len(ownerRefsWithoutUID(nil, dropUID)) != 0 {
		t.Errorf("ownerRefsWithoutUID(nil) should be empty")
	}
}
