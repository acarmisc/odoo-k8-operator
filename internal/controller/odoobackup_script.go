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

// Shell scripts run by the backup pods. They are pure functions of their arguments so
// they can be checked with `sh -n` in tests. Nothing user-controlled is spliced into the
// text: the database name, the retention, the bucket and the prefix reach the scripts
// through environment variables, and credentials come from Secret-backed variables.

import (
	"fmt"
	"strings"
)

const (
	// Environment variables read by the scripts.
	envBackupDatabase      = "BACKUP_DATABASE"
	envBackupRetention     = "BACKUP_RETENTION"
	envBackupBucket        = "BACKUP_BUCKET"
	envBackupPrefix        = "BACKUP_PREFIX"
	envBackupEndpoint      = "BACKUP_ENDPOINT"
	envBackupForcePath     = "BACKUP_FORCE_PATH_STYLE"
	envBackupRegion        = "AWS_DEFAULT_REGION"
	envAWSAccessKeyID      = "AWS_ACCESS_KEY_ID"
	envAWSSecretAccessKey  = "AWS_SECRET_ACCESS_KEY"
	envAWSConfigFile       = "AWS_CONFIG_FILE"
	envPGDatabase          = "PGDATABASE"
	envPGHost              = "PGHOST"
	envPGPort              = "PGPORT"
	envPGUser              = "PGUSER"
	secretKeyAccessKeyID   = "accessKeyID"
	secretKeySecretKey     = "secretAccessKey"
	backupAWSConfigPath    = "/tmp/aws-config"
	backupFilestoreRoot    = mountPathData + "/filestore"
	backupPVCDir           = "/backup"
	backupScratchDir       = "/scratch"
	backupManifestName     = ".files"
	backupResultPrefix     = "backup: "
	backupDumpSuffix       = ".dump"
	backupFilestoreSuffix  = "-filestore.tar.gz"
	backupDumpTimestampFmt = "%Y%m%dT%H%M%SZ"
)

// backupDumpSteps returns the shell that writes the dump (and, when includeFilestore is
// set, the filestore archive) into dir. Files are written under a dot-prefixed temporary
// name and renamed when complete. Each finished file is appended to $FILES, comma
// separated, which the caller reports.
func backupDumpSteps(dir string, includeFilestore bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "DIR=%q\n", dir)
	fmt.Fprintf(&b, "STAMP=$(date -u +%q)\n", backupDumpTimestampFmt)
	b.WriteString(`FILES=""
record() {
  FILES="${FILES:+$FILES,}$1"
}
`)
	fmt.Fprintf(&b, "DUMP=\"$%s-$STAMP%s\"\n", envBackupDatabase, backupDumpSuffix)
	b.WriteString("pg_dump --format=custom --no-owner --no-password --file \"$DIR/.$DUMP.partial\"\n")
	b.WriteString("mv \"$DIR/.$DUMP.partial\" \"$DIR/$DUMP\"\n")
	b.WriteString("record \"$DUMP\"\n")
	if includeFilestore {
		fmt.Fprintf(&b, "FS=\"$%s-$STAMP%s\"\n", envBackupDatabase, backupFilestoreSuffix)
		fmt.Fprintf(&b, "FSDIR=%s/\"$%s\"\n", backupFilestoreRoot, envBackupDatabase)
		b.WriteString(`if [ -d "$FSDIR" ]; then
  tar -C "${FSDIR%/*}" -czf "$DIR/.$FS.partial" "${FSDIR##*/}"
  mv "$DIR/.$FS.partial" "$DIR/$FS"
  record "$FS"
else
  echo "warning: filestore directory $FSDIR does not exist yet, skipping the filestore archive"
fi
`)
	}
	return b.String()
}

// backupCleanupTrap removes temporary files left behind by a failed run.
const backupCleanupTrap = `trap 'rm -f -- "$DIR"/.*.partial 2>/dev/null' EXIT
`

// buildBackupPVCScript is the whole backup for a PVC destination: dump into the claim,
// then keep only the newest retention backups of the database. retention 0 keeps all.
func buildBackupPVCScript(includeFilestore bool) string {
	var b strings.Builder
	b.WriteString("set -eu\nexport LC_ALL=C\n")
	b.WriteString(backupDumpSteps(backupPVCDir, includeFilestore))
	b.WriteString(backupCleanupTrap)
	b.WriteString(`
# Retention: names carry a UTC timestamp, so the glob sorts oldest first.
prune_dumps() {
  keep=$1
  total=0
  for f in "$DIR/$BACKUP_DATABASE"-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z.dump; do
    [ -e "$f" ] || continue
    total=$((total + 1))
  done
  drop=$((total - keep))
  i=0
  for f in "$DIR/$BACKUP_DATABASE"-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z.dump; do
    [ -e "$f" ] || continue
    i=$((i + 1))
    if [ "$i" -le "$drop" ]; then
      rm -f -- "$f" "${f%.dump}-filestore.tar.gz"
      echo "pruned: $(basename "$f")"
    fi
  done
}
`)
	fmt.Fprintf(&b, "if [ \"${%s:-0}\" -gt 0 ]; then\n  prune_dumps \"$%s\"\nfi\n", envBackupRetention, envBackupRetention)
	fmt.Fprintf(&b, "echo \"%s$FILES\"\n", backupResultPrefix)
	return b.String()
}

// buildBackupDumpScript is the init container of an S3 backup: it dumps into the
// scratch emptyDir and writes the manifest the upload container reads.
func buildBackupDumpScript(includeFilestore bool) string {
	var b strings.Builder
	b.WriteString("set -eu\nexport LC_ALL=C\n")
	b.WriteString(backupDumpSteps(backupScratchDir, includeFilestore))
	b.WriteString(backupCleanupTrap)
	fmt.Fprintf(&b, "printf '%%s' \"$FILES\" | tr ',' '\\n' > \"$DIR/%s\"\n", backupManifestName)
	b.WriteString("echo \"dumped: $FILES\"\n")
	return b.String()
}

// buildBackupUploadScript is the main container of an S3 backup. It uploads the files of
// the manifest, prunes older backups of the database when retention is set, and prints
// the result line last. Credentials and the endpoint come from the environment.
func buildBackupUploadScript() string {
	return `set -eu
export LC_ALL=C
DIR=/scratch
export AWS_CONFIG_FILE=` + backupAWSConfigPath + `
if [ "${` + envBackupForcePath + `:-false}" = "true" ]; then
  printf '[default]\ns3 =\n  addressing_style = path\n' > "$AWS_CONFIG_FILE"
fi
awscli() {
  if [ -n "${` + envBackupEndpoint + `:-}" ]; then
    aws --endpoint-url "$` + envBackupEndpoint + `" "$@"
  else
    aws "$@"
  fi
}
KEYPREFIX=""
if [ -n "${` + envBackupPrefix + `:-}" ]; then
  KEYPREFIX="${` + envBackupPrefix + `%/}/"
fi
if [ ! -s "$DIR/` + backupManifestName + `" ]; then
  echo "error: backup manifest $DIR/` + backupManifestName + ` is missing" >&2
  exit 1
fi
FILES=""
while IFS= read -r name; do
  [ -n "$name" ] || continue
  awscli s3 cp "$DIR/$name" "s3://$` + envBackupBucket + `/$KEYPREFIX$name"
  FILES="${FILES:+$FILES,}$name"
done < "$DIR/` + backupManifestName + `"

# Retention: list the database's dumps under the prefix, newest first, and delete the rest.
if [ "${` + envBackupRetention + `:-0}" -gt 0 ]; then
  awscli s3api list-objects-v2 --bucket "$` + envBackupBucket + `" \
    --prefix "$KEYPREFIX$` + envBackupDatabase + `-" --query 'Contents[].Key' --output text \
    | tr '\t' '\n' | sort -r > "$DIR/.keys"
  : > "$DIR/.dumpkeys"
  while IFS= read -r key; do
    case "$key" in
      "$KEYPREFIX$` + envBackupDatabase + `"-[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z.dump)
        echo "$key" >> "$DIR/.dumpkeys" ;;
    esac
  done < "$DIR/.keys"
  n=0
  while IFS= read -r key; do
    n=$((n + 1))
    if [ "$n" -gt "$` + envBackupRetention + `" ]; then
      awscli s3api delete-object --bucket "$` + envBackupBucket + `" --key "$key" >/dev/null
      awscli s3api delete-object --bucket "$` + envBackupBucket + `" --key "${key%.dump}-filestore.tar.gz" >/dev/null
      echo "pruned: $key"
    fi
  done < "$DIR/.dumpkeys"
fi
echo "` + backupResultPrefix + `$FILES"
`
}
