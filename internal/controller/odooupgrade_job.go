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

// Pure (cluster-free) helpers for the OdooUpgrade reconciler: naming, the odoo
// command line, node pinning and the hardened upgrade Job. Deterministic so they
// can be unit tested without an API server.

import (
	"fmt"
	"hash/fnv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	upgradeJobSuffix     = "-run"
	upgradeLabelKey      = "odoo.operator.io/upgrade"
	upgradeContainerName = "upgrade"
	upgradeMaxNameLen    = 63
	upgradeHashLen       = 8
	upgradeFallbackName  = "upgrade"

	upgradeAllModules  = "all"
	upgradeFlagDB      = "-d"
	upgradeFlagUpdate  = "-u"
	upgradeFlagInstall = "-i"
	upgradeFlagStop    = "--stop-after-init"
	upgradeFlagNoHTTP  = "--no-http"
	upgradeFlagConfig  = "--config"

	upgradeDefaultTimeout = int64(1800)
	upgradeTTLSeconds     = int32(86400)
	upgradeBackoffLimit   = int32(0)
	upgradeNodeHostLabel  = "kubernetes.io/hostname"
	upgradeCapDropAll     = "ALL"
)

// upgradePtr returns a pointer to a copy of v.
func upgradePtr[T any](v T) *T {
	return &v
}

// upgradeBoundedName returns base+suffix if it fits in maxLen, otherwise a truncated
// base followed by a short hash of base (stable for a given base) and the suffix.
func upgradeBoundedName(base, suffix string, maxLen int) string {
	if full := base + suffix; len(full) <= maxLen {
		return full
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(base))
	hash := fmt.Sprintf("%0*x", upgradeHashLen, h.Sum32())
	keep := maxLen - len(suffix) - 1 - len(hash)
	prefix := strings.Trim(base[:keep], "-.")
	if prefix == "" {
		prefix = upgradeFallbackName
	}
	return prefix + "-" + hash + suffix
}

// upgradeJobName is the Job name for an upgrade, "<name>-run" bounded to 63 characters.
func upgradeJobName(upgradeName string) string {
	return upgradeBoundedName(upgradeName, upgradeJobSuffix, upgradeMaxNameLen)
}

// upgradeLabelValue is the upgrade label value, bounded to a valid label length.
func upgradeLabelValue(upgradeName string) string {
	return upgradeBoundedName(upgradeName, "", upgradeMaxNameLen)
}

// upgradeDatabase returns the database to upgrade: the spec's, else the instance's
// postgres.database. Empty means no database is configured.
func upgradeDatabase(up *odoov1.OdooUpgrade, inst *odoov1.OdooInstance) string {
	if up.Spec.Database != "" {
		return up.Spec.Database
	}
	return inst.Spec.Postgres.Database
}

// upgradeTimeout returns activeDeadlineSeconds for the Job.
func upgradeTimeout(up *odoov1.OdooUpgrade) int64 {
	if up.Spec.TimeoutSeconds != nil {
		return *up.Spec.TimeoutSeconds
	}
	return upgradeDefaultTimeout
}

// upgradeArgs builds the odoo command line. The official image entrypoint passes
// arguments starting with '-' straight to odoo, exactly like the Deployment's args.
func upgradeArgs(database string, modules, installModules []string) []string {
	args := []string{upgradeFlagConfig, odooConfPath, upgradeFlagDB, database}
	if len(modules) > 0 {
		args = append(args, upgradeFlagUpdate, strings.Join(modules, ","))
	}
	if len(installModules) > 0 {
		args = append(args, upgradeFlagInstall, strings.Join(installModules, ","))
	}
	return append(args, upgradeFlagStop, upgradeFlagNoHTTP)
}

// upgradeAccessMode returns the access mode with the default (ReadWriteOnce) applied.
func upgradeAccessMode(mode corev1.PersistentVolumeAccessMode) corev1.PersistentVolumeAccessMode {
	if mode == "" {
		return corev1.ReadWriteOnce
	}
	return mode
}

// upgradeNeedsNodePin reports whether the upgrade pod must share a node with the
// running Odoo pod. Either RWO volume restricts mounts to one node, so the pin is
// needed unless both the addons and the data volume are ReadWriteMany.
func upgradeNeedsNodePin(spec *odoov1.OdooInstanceSpec) bool {
	return upgradeAccessMode(spec.Addons.AccessMode) != corev1.ReadWriteMany ||
		upgradeAccessMode(spec.Data.AccessMode) != corev1.ReadWriteMany
}

// upgradeEnv carries the database credentials, the same variables the Deployment uses
// (no admin password: the upgrade never starts an HTTP server).
func upgradeEnv(spec *odoov1.OdooInstanceSpec) []corev1.EnvVar {
	var env []corev1.EnvVar
	if spec.Postgres.PasswordSecret != "" {
		for _, name := range []string{envPGPassword, envDBPassword} {
			env = append(env, corev1.EnvVar{
				Name: name,
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: spec.Postgres.PasswordSecret},
						Key:                  secretKeyPassword,
					},
				},
			})
		}
	}
	return append(env, corev1.EnvVar{Name: envPGSSLMode, Value: sslModeRequire})
}

// upgradeImage returns the instance image, falling back to the operator default.
func upgradeImage(spec *odoov1.OdooInstanceSpec) string {
	if spec.Image == "" {
		return defaultOdooImage
	}
	return spec.Image
}

// upgradeLabels are set on the Job and its pod template. They must never include the
// app=odoo / instance=<name> labels: the Odoo Service selects on those.
func upgradeLabels(upgradeName string) map[string]string {
	return map[string]string{
		labelManagedBy:  managedByOperator,
		upgradeLabelKey: upgradeLabelValue(upgradeName),
	}
}

// buildUpgradeJob returns the hardened one-shot Job running the upgrade. nodeName, when
// non-empty and the volumes need it, pins the pod to that node. The caller sets the
// owner reference.
func buildUpgradeJob(up *odoov1.OdooUpgrade, inst *odoov1.OdooInstance, database, nodeName string) *batchv1.Job {
	spec := &inst.Spec
	readOnlyRoot := true
	labels := upgradeLabels(up.Name)

	podSpec := corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyNever,
		AutomountServiceAccountToken: upgradePtr(false),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: upgradePtr(true),
			RunAsUser:    upgradePtr(odooUID),
			RunAsGroup:   upgradePtr(odooUID),
			FSGroup:      upgradePtr(odooUID),
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		Containers: []corev1.Container{
			{
				Name:      upgradeContainerName,
				Image:     upgradeImage(spec),
				Args:      upgradeArgs(database, up.Spec.Modules, up.Spec.InstallModules),
				Env:       upgradeEnv(spec),
				Resources: odooResources(spec),
				VolumeMounts: []corev1.VolumeMount{
					{Name: volumeConfig, MountPath: mountPathConfig, ReadOnly: true},
					{Name: volumeAddons, MountPath: addonsMountPath, ReadOnly: true},
					{Name: volumeData, MountPath: mountPathData},
					{Name: volumeTmp, MountPath: mountPathTmp},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: upgradePtr(false),
					ReadOnlyRootFilesystem:   &readOnlyRoot,
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{upgradeCapDropAll},
					},
				},
			},
		},
		Volumes: []corev1.Volume{
			{
				Name: volumeConfig,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: configMapName(inst.Name)},
						DefaultMode:          upgradePtr(int32(0o644)),
					},
				},
			},
			{
				Name: volumeAddons,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: addonsPVCName(inst.Name),
						ReadOnly:  true,
					},
				},
			},
			{
				Name: volumeData,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: dataPVCName(inst.Name),
					},
				},
			},
			{
				Name:         volumeTmp,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			},
		},
	}

	if nodeName != "" && upgradeNeedsNodePin(spec) {
		podSpec.NodeSelector = map[string]string{upgradeNodeHostLabel: nodeName}
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      upgradeJobName(up.Name),
			Namespace: up.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            upgradePtr(upgradeBackoffLimit),
			ActiveDeadlineSeconds:   upgradePtr(upgradeTimeout(up)),
			TTLSecondsAfterFinished: upgradePtr(upgradeTTLSeconds),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec,
			},
		},
	}
}
