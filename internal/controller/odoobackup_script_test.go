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
)

const (
	btSecretValue     = "s3cr3t-value-do-not-leak"
	btFilestoreDirTxt = "/var/lib/odoo/filestore"
	btRetentionKeep   = "BACKUP_RETENTION"
	btSpellingPVC     = "PVC"
	btSpellingS3      = "S3"
	btFindingPrefix   = "script %s: "
)

// btShellSyntaxOK checks a script with `sh -n`. It skips when no sh is installed.
func btShellSyntaxOK(t *testing.T, script string) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	cmd := exec.Command(sh, "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -n failed: %v\n%s\n--- script ---\n%s", err, out, script)
	}
}

// btSecretFree fails when a credential value or a password-bearing assignment is in text.
func btSecretFree(t *testing.T, name, text string) {
	t.Helper()
	for _, bad := range []string{btSecretValue, "PGPASSWORD=", "AWS_SECRET_ACCESS_KEY=", "--password"} {
		if strings.Contains(text, bad) {
			t.Errorf(btFindingPrefix+"contains %q", name, bad)
		}
	}
}

func TestBackupScriptsAreValidShell(t *testing.T) {
	cases := []struct {
		name   string
		script string
	}{
		{"pvc with filestore, retention", buildBackupPVCScript(true)},
		{"pvc without filestore, keep all", buildBackupPVCScript(false)},
		{"s3 dump with filestore", buildBackupDumpScript(true)},
		{"s3 dump without filestore", buildBackupDumpScript(false)},
		{"s3 upload", buildBackupUploadScript()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			btShellSyntaxOK(t, tc.script)
		})
	}
}

func TestBackupPVCScript(t *testing.T) {
	withFS := buildBackupPVCScript(true)
	for _, want := range []string{
		`pg_dump --format=custom --no-owner --no-password`,
		`mv "$DIR/.$DUMP.partial" "$DIR/$DUMP"`,
		`STAMP=$(date -u +"%Y%m%dT%H%M%SZ")`,
		`DIR="/backup"`,
		`tar -C "${FSDIR%/*}" -czf`,
		btFilestoreDirTxt,
		`prune_dumps "$BACKUP_RETENTION"`,
		`echo "backup: $FILES"`,
	} {
		if !strings.Contains(withFS, want) {
			t.Errorf(btFindingPrefix+"missing %q", btSpellingPVC, want)
		}
	}
	btSecretFree(t, "pvc", withFS)

	noFS := buildBackupPVCScript(false)
	if strings.Contains(noFS, "tar -C") {
		t.Errorf("filestore archive must be absent when includeFilestore is false")
	}
	if !strings.Contains(withFS, "warning: filestore directory") {
		t.Errorf("a missing filestore directory must produce a warning line")
	}
}

func TestBackupRetentionGlobIsExact(t *testing.T) {
	script := buildBackupPVCScript(true)
	// Eight digits, T, six digits, Z: a database named "shop-2" must not match "shop" dumps.
	want := `"$DIR/$BACKUP_DATABASE"-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z.dump`
	if n := strings.Count(script, want); n != 2 {
		t.Errorf("expected the exact timestamp glob twice, found %d", n)
	}
	if !strings.Contains(script, `"${f%.dump}-filestore.tar.gz"`) {
		t.Errorf("retention must delete the matching filestore archive")
	}
}

func TestBackupDumpStepsUseTempNameThenRename(t *testing.T) {
	steps := backupDumpSteps(backupPVCDir, true)
	if !strings.Contains(steps, `.$DUMP.partial`) || !strings.Contains(steps, `.$FS.partial`) {
		t.Errorf("both files must be written under temporary names first")
	}
	if strings.Contains(backupDumpSteps(backupPVCDir, false), "$FS") {
		t.Errorf("no filestore variable expected without includeFilestore")
	}
}

func TestBackupS3DumpWritesManifest(t *testing.T) {
	script := buildBackupDumpScript(true)
	if !strings.Contains(script, `DIR="/scratch"`) {
		t.Errorf("the dump must go to the scratch directory")
	}
	if !strings.Contains(script, `"$DIR/.files"`) {
		t.Errorf("the dump must write the manifest the uploader reads")
	}
	btSecretFree(t, "s3 dump", script)
}

func TestBackupS3UploadScript(t *testing.T) {
	script := buildBackupUploadScript()
	for _, want := range []string{
		`AWS_CONFIG_FILE=/tmp/aws-config`,
		`s3 =\n  addressing_style = path`,
		`--endpoint-url "$BACKUP_ENDPOINT"`,
		`s3 cp "$DIR/$name"`,
		`list-objects-v2`,
		`delete-object`,
		`"$KEYPREFIX$BACKUP_DATABASE"-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T`,
		`echo "backup: $FILES"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf(btFindingPrefix+"missing %q", btSpellingS3, want)
		}
	}
	// The result line must be the last thing printed.
	lines := strings.Split(strings.TrimSpace(script), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, `echo "backup: `) {
		t.Errorf("last line must print the result, got %q", last)
	}
	btSecretFree(t, "s3 upload", script)
}

func TestBackupScriptsDoNotSpliceUserValues(t *testing.T) {
	// The database name, bucket, prefix and endpoint reach the scripts through environment
	// variables. No script may embed one of those values in its text.
	scripts := map[string]string{
		"pvc":    buildBackupPVCScript(true),
		"dump":   buildBackupDumpScript(true),
		"upload": buildBackupUploadScript(),
	}
	for name, s := range scripts {
		for _, forbidden := range []string{"shop-prod", "my-bucket", "https://minio"} {
			if strings.Contains(s, forbidden) {
				t.Errorf(btFindingPrefix+"embeds %q", name, forbidden)
			}
		}
	}
}
