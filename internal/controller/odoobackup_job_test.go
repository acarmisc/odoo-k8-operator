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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	btNamespace      = "odoo"
	btOtherDB        = "other"
	btInstName       = "shop"
	btBackupName     = "shop-nightly"
	btDatabase       = "shop_prod"
	btSecretName     = "backup-s3"
	btClaim          = "backups"
	btSubPath        = "shop/daily"
	btBucket         = "odoo-backups"
	btEndpoint       = "https://minio.example.internal:9000"
	btPGHost         = "pg-rw.databases.svc"
	btPGPasswdSec    = "pg-secret"
	btNodeName       = "node-7"
	btEnvPGPort      = "PGPORT"
	btEnvPGDB        = "PGDATABASE"
	btEnvRetention   = "BACKUP_RETENTION"
	btMountDest      = "/backup"
	btDefaultScratch = "10Gi"
	btSuspendTrue    = true
	btRetainKeep     = int32(5)
	btCustomPort     = int32(6432)
)

func btInstance() *odoov1.OdooInstance {
	return &odoov1.OdooInstance{
		ObjectMeta: metav1.ObjectMeta{Name: btInstName, Namespace: btNamespace},
		Spec: odoov1.OdooInstanceSpec{
			Postgres: odoov1.PostgresSpec{
				Host:           btPGHost,
				Database:       btDatabase,
				User:           "odoo",
				PasswordSecret: btPGPasswdSec,
			},
		},
	}
}

func btPVCBackup() *odoov1.OdooBackup {
	return &odoov1.OdooBackup{
		ObjectMeta: metav1.ObjectMeta{Name: btBackupName, Namespace: btNamespace},
		Spec: odoov1.OdooBackupSpec{
			InstanceRef: odoov1.OdooInstanceRef{Name: btInstName},
			Destination: odoov1.BackupDestination{
				PVC: &odoov1.BackupPVCDestination{ClaimName: btClaim, SubPath: btSubPath},
			},
			Retention: func() *int32 { v := btRetainKeep; return &v }(),
		},
	}
}

func btS3Backup() *odoov1.OdooBackup {
	return &odoov1.OdooBackup{
		ObjectMeta: metav1.ObjectMeta{Name: btBackupName, Namespace: btNamespace},
		Spec: odoov1.OdooBackupSpec{
			InstanceRef: odoov1.OdooInstanceRef{Name: btInstName},
			Destination: odoov1.BackupDestination{
				S3: &odoov1.BackupS3Destination{
					Bucket:            btBucket,
					Prefix:            "shop",
					Endpoint:          btEndpoint,
					Region:            "us-east-1",
					CredentialsSecret: btSecretName,
					ForcePathStyle:    true,
				},
			},
		},
	}
}

func btEnvValue(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func btFindVolume(volumes []corev1.Volume, name string) *corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == name {
			return &volumes[i]
		}
	}
	return nil
}

func btFindMount(mounts []corev1.VolumeMount, name string) *corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == name {
			return &mounts[i]
		}
	}
	return nil
}

func TestBackupJobSecurityPosture(t *testing.T) {
	bk := btPVCBackup()
	job := buildBackupJob(bk, btInstance(), btDatabase, "")
	spec := job.Spec.Template.Spec

	if spec.SecurityContext == nil || spec.SecurityContext.RunAsUser == nil || *spec.SecurityContext.RunAsUser != backupPodUID {
		t.Fatalf("pod must run as uid %d", backupPodUID)
	}
	if !*spec.SecurityContext.RunAsNonRoot || *spec.SecurityContext.FSGroup != odooUID {
		t.Errorf("pod must be non-root with fsGroup %d", odooUID)
	}
	if spec.SecurityContext.SeccompProfile == nil || spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("seccomp must be RuntimeDefault")
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Errorf("service account token must not be mounted")
	}
	if spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restart policy must be Never")
	}
	if *job.Spec.BackoffLimit != backupBackoffLimit {
		t.Errorf("backoffLimit must be %d", backupBackoffLimit)
	}
	if *job.Spec.ActiveDeadlineSeconds != backupDefaultTimeout {
		t.Errorf("activeDeadlineSeconds must default to %d", backupDefaultTimeout)
	}
	if *job.Spec.TTLSecondsAfterFinished != backupTTLSeconds {
		t.Errorf("ttlSecondsAfterFinished must be %d", backupTTLSeconds)
	}
	for _, c := range spec.Containers {
		sc := c.SecurityContext
		if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("%s: privilege escalation must be off", c.Name)
		}
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("%s: root filesystem must be read-only", c.Name)
		}
		if sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != backupCapDropAll {
			t.Errorf("%s: must drop ALL capabilities", c.Name)
		}
		if c.Resources.Requests == nil || c.Resources.Limits == nil {
			t.Errorf("%s: requests and limits are required", c.Name)
		}
	}
	if btFindMount(spec.Containers[0].VolumeMounts, volumeTmp) == nil {
		t.Errorf("a /tmp emptyDir must be mounted for the read-only root filesystem")
	}
	if spec.Volumes == nil || btFindVolume(spec.Volumes, volumeTmp) == nil {
		t.Errorf("the tmp volume is missing")
	}
}

func TestBackupJobLabelsNeverMatchOdooService(t *testing.T) {
	job := buildBackupJob(btPVCBackup(), btInstance(), btDatabase, "")
	for _, labels := range []map[string]string{job.Labels, job.Spec.Template.Labels} {
		if _, ok := labels[labelInstance]; ok {
			t.Errorf("backup pods must not carry the %q label", labelInstance)
		}
		if labels[labelManagedBy] != managedByOperator {
			t.Errorf("managed-by label missing")
		}
		if labels[backupLabelKey] != backupLabelValue(btBackupName) {
			t.Errorf("backup label must identify the backup")
		}
	}
}

func TestBackupPVCDestinationMounts(t *testing.T) {
	job := buildBackupJob(btPVCBackup(), btInstance(), btDatabase, "")
	spec := job.Spec.Template.Spec
	if len(spec.Containers) != 1 || len(spec.InitContainers) != 0 {
		t.Fatalf("PVC destination runs one container and no init container")
	}
	c := spec.Containers[0]
	dest := btFindMount(c.VolumeMounts, volumeDestination)
	if dest == nil || dest.MountPath != btMountDest || dest.SubPath != btSubPath {
		t.Fatalf("destination must be mounted at %s with subPath %s, got %+v", btMountDest, btSubPath, dest)
	}
	data := btFindMount(c.VolumeMounts, volumeData)
	if data == nil || !data.ReadOnly || data.MountPath != mountPathData {
		t.Fatalf("data volume must be mounted read-only at %s", mountPathData)
	}
	vol := btFindVolume(spec.Volumes, volumeData)
	if vol == nil || vol.PersistentVolumeClaim == nil || !vol.PersistentVolumeClaim.ReadOnly ||
		vol.PersistentVolumeClaim.ClaimName != dataPVCName(btInstName) {
		t.Errorf("data volume must be the read-only instance data PVC")
	}
	if btFindVolume(spec.Volumes, volumeScratch) != nil {
		t.Errorf("a PVC destination must not use the S3 scratch volume")
	}
}

func TestBackupWithoutFilestoreSkipsDataVolume(t *testing.T) {
	bk := btPVCBackup()
	bk.Spec.IncludeFilestore = func() *bool { v := false; return &v }()
	job := buildBackupJob(bk, btInstance(), btDatabase, btNodeName)
	spec := job.Spec.Template.Spec
	if btFindVolume(spec.Volumes, volumeData) != nil {
		t.Errorf("no data volume expected without the filestore")
	}
	if spec.NodeSelector != nil {
		t.Errorf("without the filestore the pod must not be pinned")
	}
}

func TestBackupFilestoreDefaultsToTrue(t *testing.T) {
	if !backupIncludeFilestore(btPVCBackup()) {
		t.Errorf("includeFilestore must default to true")
	}
}

func TestBackupNodePinRule(t *testing.T) {
	inst := btInstance()
	cases := []struct {
		name  string
		mode  corev1.PersistentVolumeAccessMode
		fs    bool
		wants bool
	}{
		{"RWO filestore pins", corev1.ReadWriteOnce, true, true},
		{"unset access mode pins", "", true, true},
		{"RWX filestore does not pin", corev1.ReadWriteMany, true, false},
		{"no filestore never pins", corev1.ReadWriteOnce, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst.Spec.Data.AccessMode = tc.mode
			if got := backupNeedsNodePin(&inst.Spec, tc.fs); got != tc.wants {
				t.Fatalf("backupNeedsNodePin = %v, want %v", got, tc.wants)
			}
			job := buildBackupJob(btPVCBackup(), inst, btDatabase, btNodeName)
			pinned := job.Spec.Template.Spec.NodeSelector[backupNodeHostLabel] == btNodeName
			if tc.fs && pinned != tc.wants {
				t.Errorf("pod pin = %v, want %v", pinned, tc.wants)
			}
		})
	}
}

func TestBackupPGEnvNeverCarriesThePassword(t *testing.T) {
	job := buildBackupJob(btPVCBackup(), btInstance(), btDatabase, "")
	main := job.Spec.Template.Spec.Containers[0]
	if v, _ := btEnvValue(main.Env, btEnvPGDB); v != btDatabase {
		t.Errorf("PGDATABASE = %q", v)
	}
	if v, _ := btEnvValue(main.Env, "PGHOST"); v != btPGHost {
		t.Errorf("PGHOST = %q", v)
	}
	if v, _ := btEnvValue(main.Env, btEnvPGPort); v != "5432" {
		t.Errorf("PGPORT default = %q", v)
	}
	var pw *corev1.EnvVar
	for i := range main.Env {
		if main.Env[i].Name == envPGPassword {
			pw = &main.Env[i]
		}
	}
	if pw == nil || pw.ValueFrom == nil || pw.ValueFrom.SecretKeyRef == nil ||
		pw.ValueFrom.SecretKeyRef.Name != btPGPasswdSec || pw.ValueFrom.SecretKeyRef.Key != secretKeyPassword {
		t.Fatalf("PGPASSWORD must come from the Secret key %q", secretKeyPassword)
	}
	if v, _ := btEnvValue(main.Env, envPGSSLMode); v != sslModeRequire {
		t.Errorf("PGSSLMODE must be %s", sslModeRequire)
	}
	for _, arg := range append(main.Command, main.Args...) {
		if strings.Contains(arg, btSecretName) {
			t.Errorf("no secret name may appear in the command line")
		}
	}
}

func TestBackupPGPortOverride(t *testing.T) {
	inst := btInstance()
	inst.Spec.Postgres.Port = btCustomPort
	env := backupPGEnv(&inst.Spec, btDatabase)
	if v, _ := btEnvValue(env, btEnvPGPort); v != "6432" {
		t.Errorf("PGPORT = %q, want 6432", v)
	}
}

func TestBackupS3TwoContainers(t *testing.T) {
	bk := btS3Backup()
	job := buildBackupJob(bk, btInstance(), btDatabase, "")
	spec := job.Spec.Template.Spec
	if len(spec.InitContainers) != 1 || len(spec.Containers) != 1 {
		t.Fatalf("S3 runs an init dump container and an upload container")
	}
	dump, upload := spec.InitContainers[0], spec.Containers[0]

	if _, ok := btEnvValue(dump.Env, envAWSAccessKeyID); ok {
		t.Errorf("the dump container must not hold S3 credentials")
	}
	if _, ok := btEnvValue(upload.Env, envPGPassword); ok {
		// The uploader must not see the database password.
		t.Errorf("the upload container must not hold the database password")
	}
	for _, name := range []string{envAWSAccessKeyID, envAWSSecretAccessKey} {
		var e *corev1.EnvVar
		for i := range upload.Env {
			if upload.Env[i].Name == name {
				e = &upload.Env[i]
			}
		}
		if e == nil || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil ||
			e.ValueFrom.SecretKeyRef.Name != btSecretName {
			t.Errorf("%s must come from the credentials Secret", name)
		}
	}
	if v, _ := btEnvValue(upload.Env, envBackupForcePath); v != backupEnvTrue {
		t.Errorf("forcePathStyle must reach the uploader as %s", backupEnvTrue)
	}
	if v, _ := btEnvValue(upload.Env, envBackupEndpoint); v != btEndpoint {
		t.Errorf("endpoint = %q", v)
	}
	if v, _ := btEnvValue(upload.Env, envBackupRegion); v != "us-east-1" {
		t.Errorf("region = %q", v)
	}
	if upload.Image != backupS3Image() {
		t.Errorf("upload image = %q, want %q", upload.Image, backupS3Image())
	}
	scratch := btFindVolume(spec.Volumes, volumeScratch)
	if scratch == nil || scratch.EmptyDir == nil || scratch.EmptyDir.SizeLimit == nil ||
		scratch.EmptyDir.SizeLimit.Cmp(resource.MustParse(btDefaultScratch)) != 0 {
		t.Errorf("scratch emptyDir must be bounded by scratchSize (default 10Gi)")
	}
	if btFindMount(dump.VolumeMounts, volumeScratch) == nil || btFindMount(upload.VolumeMounts, volumeScratch) == nil {
		t.Errorf("both containers must see the scratch directory")
	}
	if spec.Containers[0].Name != backupMainContainer || spec.InitContainers[0].Name != backupInitContainer {
		t.Errorf("container names must be %q and %q", backupMainContainer, backupInitContainer)
	}
}

func TestBackupScratchSizeOverride(t *testing.T) {
	bk := btS3Backup()
	bk.Spec.ScratchSize = "50Gi"
	job := buildBackupJob(bk, btInstance(), btDatabase, "")
	scratch := btFindVolume(job.Spec.Template.Spec.Volumes, volumeScratch)
	if scratch.EmptyDir.SizeLimit.Cmp(resource.MustParse("50Gi")) != 0 {
		t.Errorf("scratch size = %s, want 50Gi", scratch.EmptyDir.SizeLimit.String())
	}
}

func TestBackupImagesFromEnvironment(t *testing.T) {
	t.Setenv(envBackupImage, "registry.example/pg:17")
	t.Setenv(envS3Image, "registry.example/aws:2")
	if backupImage() != "registry.example/pg:17" || backupS3Image() != "registry.example/aws:2" {
		t.Errorf("images must follow the environment overrides")
	}
	t.Setenv(envBackupImage, "")
	t.Setenv(envS3Image, "")
	if backupImage() != defaultBackupImg || backupS3Image() != defaultS3Image {
		t.Errorf("images must fall back to the defaults")
	}
}

func TestBackupDatabaseDefault(t *testing.T) {
	bk := btPVCBackup()
	if got := backupDatabase(bk, btInstance()); got != btDatabase {
		t.Errorf("database defaults to the instance's, got %q", got)
	}
	bk.Spec.Database = btOtherDB
	if got := backupDatabase(bk, btInstance()); got != btOtherDB {
		t.Errorf("spec database must win, got %q", got)
	}
}

func TestBackupTimeoutAndRetentionDefaults(t *testing.T) {
	bk := btPVCBackup()
	bk.Spec.Retention = nil
	if backupRetention(bk) != 0 {
		t.Errorf("unset retention keeps everything (0)")
	}
	bk.Spec.TimeoutSeconds = func() *int64 { v := int64(120); return &v }()
	if backupTimeout(bk) != 120 {
		t.Errorf("explicit timeout ignored")
	}
}

func TestBackupCronJobShape(t *testing.T) {
	bk := btPVCBackup()
	bk.Spec.Schedule = "0 2 * * *"
	bk.Spec.Suspend = btSuspendTrue
	cron := buildBackupCronJob(bk, btInstance(), btDatabase, "")
	if cron.Spec.Schedule != "0 2 * * *" {
		t.Errorf("schedule = %q", cron.Spec.Schedule)
	}
	if cron.Spec.ConcurrencyPolicy != "Forbid" {
		t.Errorf("concurrencyPolicy must be Forbid, got %q", cron.Spec.ConcurrencyPolicy)
	}
	if *cron.Spec.SuccessfulJobsHistoryLimit != backupHistoryLimit || *cron.Spec.FailedJobsHistoryLimit != backupHistoryLimit {
		t.Errorf("history limits must be %d", backupHistoryLimit)
	}
	if *cron.Spec.StartingDeadlineSeconds != backupStartingDeadline {
		t.Errorf("startingDeadlineSeconds must be %d", backupStartingDeadline)
	}
	if !*cron.Spec.Suspend {
		t.Errorf("suspend must follow the spec")
	}
	if cron.Annotations[backupHashAnnotation] == "" {
		t.Errorf("the spec hash annotation is required")
	}
	if cron.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Name != backupMainContainer {
		t.Errorf("the CronJob template must use the shared pod spec")
	}
}

func TestBackupSpecHashChangesWithSpec(t *testing.T) {
	a := buildBackupCronJob(btPVCBackup(), btInstance(), btDatabase, "")
	bk := btPVCBackup()
	bk.Spec.Destination.PVC.ClaimName = "other"
	b := buildBackupCronJob(bk, btInstance(), btDatabase, "")
	if a.Annotations[backupHashAnnotation] == b.Annotations[backupHashAnnotation] {
		t.Errorf("a destination change must change the hash")
	}
	c := buildBackupCronJob(btPVCBackup(), btInstance(), btDatabase, "")
	if a.Annotations[backupHashAnnotation] != c.Annotations[backupHashAnnotation] {
		t.Errorf("the hash must be stable for an identical spec")
	}
}

func TestBackupNamesAreBounded(t *testing.T) {
	long := strings.Repeat("a", 80)
	if n := backupJobName(long); len(n) > backupMaxNameLen {
		t.Errorf("job name too long: %d", len(n))
	}
	if n := backupCronJobName(long); len(n) > backupMaxCronNameLen {
		t.Errorf("cronjob name too long: %d", len(n))
	}
	if backupJobName("shop") != "shop-run" {
		t.Errorf("short job name = %q", backupJobName("shop"))
	}
	if backupCronJobName("shop") != "shop-sched" {
		t.Errorf("short cronjob name = %q", backupCronJobName("shop"))
	}
}

func TestBackupParseResult(t *testing.T) {
	cases := []struct {
		name string
		logs string
		want string
	}{
		{"single dump", "starting\nbackup: shop-20261017T020000Z.dump\n", "shop-20261017T020000Z.dump"},
		{"dump and filestore", "x\nbackup: a.dump,a-filestore.tar.gz", "a.dump,a-filestore.tar.gz"},
		{"last result wins", "backup: old.dump\nbackup: new.dump\n", "new.dump"},
		{"no result line", "pg_dump: error\n", ""},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := backupParseResult(tc.logs); got != tc.want {
				t.Errorf("backupParseResult = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBackupValidateSpec(t *testing.T) {
	ok := func() *odoov1.OdooBackup { return btPVCBackup() }
	bad := []struct {
		name   string
		mutate func(*odoov1.OdooBackup)
	}{
		{"missing instance", func(b *odoov1.OdooBackup) { b.Spec.InstanceRef.Name = "" }},
		{"foreign namespace", func(b *odoov1.OdooBackup) { b.Spec.InstanceRef.Namespace = "elsewhere" }},
		{"bad database", func(b *odoov1.OdooBackup) { b.Spec.Database = "a b" }},
		{"no destination", func(b *odoov1.OdooBackup) { b.Spec.Destination = odoov1.BackupDestination{} }},
		{"both destinations", func(b *odoov1.OdooBackup) {
			b.Spec.Destination.S3 = &odoov1.BackupS3Destination{Bucket: btBucket, CredentialsSecret: btSecretName}
		}},
		{"empty claim", func(b *odoov1.OdooBackup) { b.Spec.Destination.PVC.ClaimName = "" }},
		{"subPath with dotdot", func(b *odoov1.OdooBackup) { b.Spec.Destination.PVC.SubPath = "a/../b" }},
		{"subPath absolute", func(b *odoov1.OdooBackup) { b.Spec.Destination.PVC.SubPath = "/etc" }},
		{"subPath empty segment", func(b *odoov1.OdooBackup) { b.Spec.Destination.PVC.SubPath = "a//b" }},
		{"subPath bad charset", func(b *odoov1.OdooBackup) { b.Spec.Destination.PVC.SubPath = "a b" }},
		{"retention zero", func(b *odoov1.OdooBackup) { v := int32(0); b.Spec.Retention = &v }},
		{"scratch not a quantity", func(b *odoov1.OdooBackup) { b.Spec.ScratchSize = "lots" }},
		{"timeout too small", func(b *odoov1.OdooBackup) { v := int64(10); b.Spec.TimeoutSeconds = &v }},
		{"schedule with six fields", func(b *odoov1.OdooBackup) { b.Spec.Schedule = "0 2 * * * *" }},
	}
	if err := backupValidateSpec(ok()); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			bk := ok()
			tc.mutate(bk)
			if err := backupValidateSpec(bk); err == nil {
				t.Errorf("expected a validation error")
			}
		})
	}
}

func TestBackupValidateS3(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*odoov1.BackupS3Destination)
		wantErr bool
	}{
		{"ok", func(*odoov1.BackupS3Destination) {}, false},
		{"bucket uppercase", func(s *odoov1.BackupS3Destination) { s.Bucket = "Bad_Bucket" }, true},
		{"prefix leading slash", func(s *odoov1.BackupS3Destination) { s.Prefix = "/x" }, true},
		{"prefix dotdot", func(s *odoov1.BackupS3Destination) { s.Prefix = "x/.." }, true},
		{"endpoint without scheme", func(s *odoov1.BackupS3Destination) { s.Endpoint = "minio:9000" }, true},
		{"no credentials secret", func(s *odoov1.BackupS3Destination) { s.CredentialsSecret = "" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bk := btS3Backup()
			tc.mutate(bk.Spec.Destination.S3)
			err := backupValidateSpec(bk)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
