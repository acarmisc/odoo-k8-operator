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

// Pure (cluster-free) helpers for the OdooBackup reconciler: naming, the hardened pod
// template shared by the one-shot Job and the CronJob, and the log parsing. Deterministic
// so they can be unit tested without an API server.

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	envBackupImage   = "OPERATOR_BACKUP_IMAGE"
	envS3Image       = "OPERATOR_S3_IMAGE"
	defaultBackupImg = "postgres:17-alpine"
	defaultS3Image   = "amazon/aws-cli:2.27.50"

	backupJobSuffix      = "-run"
	backupCronSuffix     = "-sched"
	backupLabelKey       = "odoo.operator.io/backup"
	backupHashAnnotation = "odoo.operator.io/backup-hash"
	backupMaxNameLen     = 63
	backupMaxCronNameLen = 52
	backupHashLen        = 8
	backupFallbackName   = "backup"

	backupInitContainer = "dump"
	backupMainContainer = "backup"
	backupVolumeScratch = "scratch"
	volumeDestination   = "destination"
	backupPGPortDefault = "5432"
	backupHome          = "/tmp"
	backupShell         = "/bin/sh"

	backupDefaultTimeout   = int64(3600)
	backupDefaultScratch   = "10Gi"
	backupTTLSeconds       = int32(86400)
	backupBackoffLimit     = int32(0)
	backupHistoryLimit     = int32(3)
	backupStartingDeadline = int64(300)
	backupNodeHostLabel    = "kubernetes.io/hostname"
	backupCapDropAll       = "ALL"
	backupPodUID           = int64(70) // the postgres user of the official alpine images
	backupFSGroup          = odooUID   // the odoo group, which owns the data PVC
	backupDefaultMemory    = "256Mi"
	backupDefaultCPU       = "100m"
	backupLimitMemory      = "1Gi"
	backupLimitCPU         = "1"
	backupEnvTrue          = "true"
	backupEnvFalse         = "false"
)

// backupImage returns the image that runs pg_dump, overridable with OPERATOR_BACKUP_IMAGE.
func backupImage() string {
	if v := os.Getenv(envBackupImage); v != "" {
		return v
	}
	return defaultBackupImg
}

// backupS3Image returns the image that uploads to S3, overridable with OPERATOR_S3_IMAGE.
func backupS3Image() string {
	if v := os.Getenv(envS3Image); v != "" {
		return v
	}
	return defaultS3Image
}

// backupBoundedName returns base+suffix if it fits in maxLen, otherwise a truncated base,
// a short hash of base (stable for a given base) and the suffix.
func backupBoundedName(base, suffix string, maxLen int) string {
	if full := base + suffix; len(full) <= maxLen {
		return full
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(base))
	hash := fmt.Sprintf("%0*x", backupHashLen, h.Sum32())
	keep := maxLen - len(suffix) - 1 - len(hash)
	prefix := strings.Trim(base[:keep], "-.")
	if prefix == "" {
		prefix = backupFallbackName
	}
	return prefix + "-" + hash + suffix
}

// backupJobName is the one-shot Job name, "<name>-run" bounded to 63 characters.
func backupJobName(backupName string) string {
	return backupBoundedName(backupName, backupJobSuffix, backupMaxNameLen)
}

// backupCronJobName is the CronJob name, "<name>-sched" bounded to 52 characters (the
// limit for CronJob names, which leaves room for the Job suffix Kubernetes adds).
func backupCronJobName(backupName string) string {
	return backupBoundedName(backupName, backupCronSuffix, backupMaxCronNameLen)
}

// backupLabelValue is the backup label value, bounded to a valid label length.
func backupLabelValue(backupName string) string {
	return backupBoundedName(backupName, "", backupMaxNameLen)
}

// backupLabels are set on the Job, the CronJob and their pod templates. They must never
// include the app=odoo / instance=<name> labels: the Odoo Service selects on those.
func backupLabels(backupName string) map[string]string {
	return map[string]string{
		labelManagedBy: managedByOperator,
		backupLabelKey: backupLabelValue(backupName),
	}
}

// backupDatabase returns the database to dump: the spec's, else the instance's
// postgres.database. Empty means no database is configured.
func backupDatabase(bk *odoov1.OdooBackup, inst *odoov1.OdooInstance) string {
	if bk.Spec.Database != "" {
		return bk.Spec.Database
	}
	return inst.Spec.Postgres.Database
}

// backupIncludeFilestore reports whether the filestore is archived. It defaults to true.
func backupIncludeFilestore(bk *odoov1.OdooBackup) bool {
	if bk.Spec.IncludeFilestore == nil {
		return true
	}
	return *bk.Spec.IncludeFilestore
}

// backupRetention returns the number of backups to keep, or 0 to keep all of them.
func backupRetention(bk *odoov1.OdooBackup) int32 {
	if bk.Spec.Retention == nil {
		return 0
	}
	return *bk.Spec.Retention
}

// backupTimeout returns activeDeadlineSeconds for the backup pod.
func backupTimeout(bk *odoov1.OdooBackup) int64 {
	if bk.Spec.TimeoutSeconds != nil {
		return *bk.Spec.TimeoutSeconds
	}
	return backupDefaultTimeout
}

// backupScratchQuantity returns the emptyDir size limit for the S3 scratch space.
func backupScratchQuantity(bk *odoov1.OdooBackup) resource.Quantity {
	raw := bk.Spec.ScratchSize
	if raw == "" {
		raw = backupDefaultScratch
	}
	q, err := resource.ParseQuantity(raw)
	if err != nil {
		q = resource.MustParse(backupDefaultScratch)
	}
	return q
}

// backupAccessMode returns the access mode with the default (ReadWriteOnce) applied.
func backupAccessMode(mode corev1.PersistentVolumeAccessMode) corev1.PersistentVolumeAccessMode {
	if mode == "" {
		return corev1.ReadWriteOnce
	}
	return mode
}

// backupNeedsNodePin reports whether the backup pod must share a node with the running
// Odoo pod. Only the data volume matters: it is mounted by the pod when the filestore is
// included, and a ReadWriteOnce volume can be mounted from one node only.
func backupNeedsNodePin(spec *odoov1.OdooInstanceSpec, includeFilestore bool) bool {
	return includeFilestore && backupAccessMode(spec.Data.AccessMode) != corev1.ReadWriteMany
}

// backupPGEnv carries the connection settings for pg_dump. The password comes from the
// instance's Secret and is never part of the command line or the script text.
func backupPGEnv(spec *odoov1.OdooInstanceSpec, database string) []corev1.EnvVar {
	env := []corev1.EnvVar{{Name: envPGDatabase, Value: database}}
	if spec.Postgres.Host != "" {
		env = append(env, corev1.EnvVar{Name: envPGHost, Value: spec.Postgres.Host})
	}
	port := backupPGPortDefault
	if spec.Postgres.Port > 0 {
		port = strconv.Itoa(int(spec.Postgres.Port))
	}
	env = append(env, corev1.EnvVar{Name: envPGPort, Value: port})
	if spec.Postgres.User != "" {
		env = append(env, corev1.EnvVar{Name: envPGUser, Value: spec.Postgres.User})
	}
	if spec.Postgres.PasswordSecret != "" {
		env = append(env, secretEnv(envPGPassword, spec.Postgres.PasswordSecret, secretKeyPassword))
	}
	return append(env, corev1.EnvVar{Name: envPGSSLMode, Value: sslModeRequire})
}

// secretEnv returns an environment variable read from a Secret key.
func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret},
				Key:                  key,
			},
		},
	}
}

// backupS3Env is the environment of the S3 upload container: the bucket settings and the
// credentials. HOME points at the writable /tmp because the root filesystem is read-only.
func backupS3Env(s3 *odoov1.BackupS3Destination, retention int32, database string) []corev1.EnvVar {
	forcePath := backupEnvFalse
	if s3.ForcePathStyle {
		forcePath = backupEnvTrue
	}
	env := []corev1.EnvVar{
		{Name: envBackupDatabase, Value: database},
		{Name: envBackupRetention, Value: strconv.Itoa(int(retention))},
		{Name: envBackupBucket, Value: s3.Bucket},
		{Name: envBackupForcePath, Value: forcePath},
		{Name: "HOME", Value: backupHome},
		secretEnv(envAWSAccessKeyID, s3.CredentialsSecret, secretKeyAccessKeyID),
		secretEnv(envAWSSecretAccessKey, s3.CredentialsSecret, secretKeySecretKey),
	}
	if s3.Prefix != "" {
		env = append(env, corev1.EnvVar{Name: envBackupPrefix, Value: s3.Prefix})
	}
	if s3.Endpoint != "" {
		env = append(env, corev1.EnvVar{Name: envBackupEndpoint, Value: s3.Endpoint})
	}
	if s3.Region != "" {
		env = append(env, corev1.EnvVar{Name: envBackupRegion, Value: s3.Region})
	}
	return env
}

// backupContainerSecurity is the hardened security context shared by every backup container.
func backupContainerSecurity() *corev1.SecurityContext {
	readOnlyRoot := true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: upgradePtr(false),
		ReadOnlyRootFilesystem:   &readOnlyRoot,
		RunAsNonRoot:             upgradePtr(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{backupCapDropAll},
		},
	}
}

// backupPodSecurity is the pod-level security context. The UID matches the postgres user
// of the official image; the fsGroup lets the pod read the Odoo data volume and write a
// PVC destination.
func backupPodSecurity() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: upgradePtr(true),
		RunAsUser:    upgradePtr(backupPodUID),
		RunAsGroup:   upgradePtr(backupPodUID),
		FSGroup:      upgradePtr(backupFSGroup),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// backupResources returns the requests and limits of each backup container.
func backupResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(backupDefaultCPU),
			corev1.ResourceMemory: resource.MustParse(backupDefaultMemory),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(backupLimitCPU),
			corev1.ResourceMemory: resource.MustParse(backupLimitMemory),
		},
	}
}

// buildBackupPodSpec returns the pod template spec of a backup. A PVC destination runs a
// single container that dumps and prunes. An S3 destination runs an init container that
// dumps into an emptyDir, then an upload container that holds the credentials. nodeName,
// when non-empty and the volumes need it, pins the pod to that node.
func buildBackupPodSpec(bk *odoov1.OdooBackup, inst *odoov1.OdooInstance, database, nodeName string) corev1.PodSpec {
	spec := &inst.Spec
	includeFS := backupIncludeFilestore(bk)
	retention := backupRetention(bk)

	var volumes []corev1.Volume
	var containers []corev1.Container
	var initContainers []corev1.Container

	mounts := []corev1.VolumeMount{{Name: volumeTmp, MountPath: mountPathTmp}}
	volumes = append(volumes, corev1.Volume{
		Name:         volumeTmp,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	if includeFS {
		volumes = append(volumes, corev1.Volume{
			Name: volumeData,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: dataPVCName(inst.Name),
					ReadOnly:  true,
				},
			},
		})
	}
	dataMount := func() []corev1.VolumeMount {
		if !includeFS {
			return nil
		}
		return []corev1.VolumeMount{{Name: volumeData, MountPath: mountPathData, ReadOnly: true}}
	}

	if bk.Spec.Destination.PVC != nil {
		pvc := bk.Spec.Destination.PVC
		volumes = append(volumes, corev1.Volume{
			Name: volumeDestination,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvc.ClaimName},
			},
		})
		m := append([]corev1.VolumeMount{}, dataMount()...)
		m = append(m, corev1.VolumeMount{Name: volumeDestination, MountPath: backupPVCDir, SubPath: pvc.SubPath})
		m = append(m, mounts...)
		env := append(backupPGEnv(spec, database),
			corev1.EnvVar{Name: envBackupDatabase, Value: database},
			corev1.EnvVar{Name: envBackupRetention, Value: strconv.Itoa(int(retention))},
		)
		containers = append(containers, corev1.Container{
			Name:            backupMainContainer,
			Image:           backupImage(),
			Command:         []string{backupShell, "-c", buildBackupPVCScript(includeFS)},
			Env:             env,
			Resources:       backupResources(),
			VolumeMounts:    m,
			SecurityContext: backupContainerSecurity(),
		})
	} else {
		s3 := bk.Spec.Destination.S3
		size := backupScratchQuantity(bk)
		volumes = append(volumes, corev1.Volume{
			Name: backupVolumeScratch,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size},
			},
		})
		scratchMount := corev1.VolumeMount{Name: backupVolumeScratch, MountPath: backupScratchDir}
		dumpMounts := append(append([]corev1.VolumeMount{}, dataMount()...), scratchMount)
		dumpMounts = append(dumpMounts, mounts...)
		initContainers = append(initContainers, corev1.Container{
			Name:            backupInitContainer,
			Image:           backupImage(),
			Command:         []string{backupShell, "-c", buildBackupDumpScript(includeFS)},
			Env:             append(backupPGEnv(spec, database), corev1.EnvVar{Name: envBackupDatabase, Value: database}),
			Resources:       backupResources(),
			VolumeMounts:    dumpMounts,
			SecurityContext: backupContainerSecurity(),
		})
		uploadMounts := append([]corev1.VolumeMount{scratchMount}, mounts...)
		containers = append(containers, corev1.Container{
			Name:            backupMainContainer,
			Image:           backupS3Image(),
			Command:         []string{backupShell, "-c", buildBackupUploadScript()},
			Env:             backupS3Env(s3, retention, database),
			Resources:       backupResources(),
			VolumeMounts:    uploadMounts,
			SecurityContext: backupContainerSecurity(),
		})
	}

	podSpec := corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyNever,
		AutomountServiceAccountToken: upgradePtr(false),
		SecurityContext:              backupPodSecurity(),
		InitContainers:               initContainers,
		Containers:                   containers,
		Volumes:                      volumes,
	}
	if nodeName != "" && backupNeedsNodePin(spec, includeFS) {
		podSpec.NodeSelector = map[string]string{backupNodeHostLabel: nodeName}
	}
	return podSpec
}

// buildBackupJobSpec is the Job spec shared by the one-shot Job and the CronJob template.
func buildBackupJobSpec(bk *odoov1.OdooBackup, inst *odoov1.OdooInstance, database, nodeName string) batchv1.JobSpec {
	return batchv1.JobSpec{
		BackoffLimit:            upgradePtr(backupBackoffLimit),
		ActiveDeadlineSeconds:   upgradePtr(backupTimeout(bk)),
		TTLSecondsAfterFinished: upgradePtr(backupTTLSeconds),
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: backupLabels(bk.Name)},
			Spec:       buildBackupPodSpec(bk, inst, database, nodeName),
		},
	}
}

// buildBackupJob returns the one-shot Job. The caller sets the owner reference.
func buildBackupJob(bk *odoov1.OdooBackup, inst *odoov1.OdooInstance, database, nodeName string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupJobName(bk.Name),
			Namespace: bk.Namespace,
			Labels:    backupLabels(bk.Name),
		},
		Spec: buildBackupJobSpec(bk, inst, database, nodeName),
	}
}

// buildBackupCronJob returns the CronJob of a scheduled backup. The pod template is the
// one-shot template, so both paths share the hardening. The hash annotation lets the
// reconciler tell whether the CronJob is already up to date.
func buildBackupCronJob(bk *odoov1.OdooBackup, inst *odoov1.OdooInstance, database, nodeName string) *batchv1.CronJob {
	jobSpec := buildBackupJobSpec(bk, inst, database, nodeName)
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:        backupCronJobName(bk.Name),
			Namespace:   bk.Namespace,
			Labels:      backupLabels(bk.Name),
			Annotations: map[string]string{backupHashAnnotation: backupSpecHash(jobSpec)},
		},
		Spec: batchv1.CronJobSpec{
			Schedule:                   bk.Spec.Schedule,
			StartingDeadlineSeconds:    upgradePtr(backupStartingDeadline),
			ConcurrencyPolicy:          batchv1.ForbidConcurrent,
			Suspend:                    upgradePtr(bk.Spec.Suspend),
			SuccessfulJobsHistoryLimit: upgradePtr(backupHistoryLimit),
			FailedJobsHistoryLimit:     upgradePtr(backupHistoryLimit),
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: backupLabels(bk.Name)},
				Spec:       jobSpec,
			},
		},
	}
}

// backupSpecHash returns a stable short hash of v's JSON form.
func backupSpecHash(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	h := fnv.New32a()
	_, _ = h.Write(raw)
	return fmt.Sprintf("%08x", h.Sum32())
}

// backupParseResult returns the file list from the last "backup: <files>" line of a pod
// log, or "" when there is none.
func backupParseResult(logs string) string {
	result := ""
	for _, line := range strings.Split(logs, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), backupResultPrefix); ok {
			result = strings.TrimSpace(rest)
		}
	}
	return result
}
