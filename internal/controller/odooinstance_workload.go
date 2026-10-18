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
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

func boolPtr(b bool) *bool    { return &b }
func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }

func desiredReplicas(spec *odoov1.OdooInstanceSpec) int32 {
	if spec.Replicas < 1 {
		return 1
	}
	return spec.Replicas
}

func odooImage(spec *odoov1.OdooInstanceSpec) string {
	if spec.Image == "" {
		return defaultOdooImage
	}
	return spec.Image
}

// selectorLabels are immutable once a Deployment exists; do not extend them.
func selectorLabels(instanceName string) map[string]string {
	return map[string]string{
		labelApp:      appOdoo,
		labelInstance: instanceName,
	}
}

// podLabels are the selector labels plus the standard Kubernetes labels.
func podLabels(instanceName string) map[string]string {
	labels := selectorLabels(instanceName)
	labels[labelAppName] = appOdoo
	labels[labelAppInstance] = instanceName
	labels[labelManagedBy] = managedByOperator
	return labels
}

// deploymentStrategy uses Recreate for a single replica on a volume that is
// not ReadWriteMany: an RWO volume cannot be attached to a surge pod that
// lands on another node. Otherwise it rolls with no unavailable pods.
func deploymentStrategy(replicas int32, dataAccessMode corev1.PersistentVolumeAccessMode) appsv1.DeploymentStrategy {
	if replicas == 1 && dataAccessMode != corev1.ReadWriteMany {
		return appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	}
	maxUnavailable := intstr.FromInt32(0)
	maxSurge := intstr.FromInt32(1)
	return appsv1.DeploymentStrategy{
		Type: appsv1.RollingUpdateDeploymentStrategyType,
		RollingUpdate: &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &maxUnavailable,
			MaxSurge:       &maxSurge,
		},
	}
}

func buildOdooArgs(instance *odoov1.OdooInstance) []string {
	// PGPASSWORD env var handles database auth via libpq; no --db-password arg.
	args := []string{upgradeFlagConfig, odooConfPath}
	if instance.Spec.AdminPasswordSecret != "" {
		// Kubernetes expands $(VAR) from the container env, so the password
		// never appears in the pod spec or the ConfigMap.
		args = append(args, fmt.Sprintf("--admin-passwd=$(%s)", envAdminPasswd))
	}
	return args
}

func secretKeyEnv(envName, secretName string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  secretKeyPassword,
			},
		},
	}
}

func buildOdooEnv(spec *odoov1.OdooInstanceSpec) []corev1.EnvVar {
	var env []corev1.EnvVar
	if spec.Postgres.PasswordSecret != "" {
		env = append(env,
			secretKeyEnv(envPGPassword, spec.Postgres.PasswordSecret),
			secretKeyEnv(envDBPassword, spec.Postgres.PasswordSecret),
		)
	}
	if spec.Postgres.Database != "" {
		env = append(env, corev1.EnvVar{Name: envPGSSLMode, Value: sslModeRequire})
	}
	if spec.AdminPasswordSecret != "" {
		env = append(env, secretKeyEnv(envAdminPasswd, spec.AdminPasswordSecret))
	}
	return env
}

func odooResources(spec *odoov1.OdooInstanceSpec) corev1.ResourceRequirements {
	if spec.Resources.Requests != nil || spec.Resources.Limits != nil {
		return spec.Resources
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
}

// odooProbes returns startup, readiness and liveness probes against Odoo's
// health endpoint. Timeout and threshold fields are set explicitly to the
// API server defaults so the rendered spec matches what is stored.
func odooProbes() (startup, readiness, liveness *corev1.Probe) {
	probe := func(period, failure int32) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   healthPath,
					Port:   intstr.FromInt32(portOdoo),
					Scheme: corev1.URISchemeHTTP,
				},
			},
			PeriodSeconds:    period,
			TimeoutSeconds:   1,
			SuccessThreshold: 1,
			FailureThreshold: failure,
		}
	}
	return probe(10, 30), probe(10, 3), probe(20, 3)
}

func buildServicePorts() []corev1.ServicePort {
	return []corev1.ServicePort{
		{
			Name:       portNameOdoo,
			Port:       portOdoo,
			TargetPort: intstr.FromInt32(portOdoo),
			Protocol:   corev1.ProtocolTCP,
		},
		{
			Name:       portNameLongpolling,
			Port:       portLongpolling,
			TargetPort: intstr.FromInt32(portLongpolling),
			Protocol:   corev1.ProtocolTCP,
		},
	}
}

func configMapName(instanceName string) string {
	return instanceName + suffixConfigMap
}

// podAnnotations are the rollout hashes stamped on every pod template of the instance. The
// web and cron pods carry the same values, so they roll together.
func podAnnotations(hashes rolloutHashes) map[string]string {
	return map[string]string{
		annotationConfigHash:  hashes.config,
		annotationSecretsHash: hashes.secrets,
		annotationAddonsHash:  hashes.addons,
	}
}

func odooPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: boolPtr(true),
		RunAsUser:    int64Ptr(odooUID),
		FSGroup:      int64Ptr(odooUID),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

func odooContainerSecurityContext() *corev1.SecurityContext {
	readOnly := true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		ReadOnlyRootFilesystem:   &readOnly,
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// odooVolumeMounts are the mounts of the Odoo container, web and cron alike.
func odooVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: volumeConfig, MountPath: mountPathConfig, ReadOnly: true},
		{Name: volumeAddons, MountPath: addonsMountPath, ReadOnly: true},
		{Name: volumeData, MountPath: mountPathData},
		{Name: volumeTmp, MountPath: mountPathTmp},
	}
}

// odooVolumes are the pod volumes of the instance, web and cron alike.
func odooVolumes(instanceName string) []corev1.Volume {
	return []corev1.Volume{
		{
			Name: volumeConfig,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: configMapName(instanceName)},
					DefaultMode:          int32Ptr(0o644),
				},
			},
		},
		{
			Name: volumeAddons,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: addonsPVCName(instanceName),
					ReadOnly:  true,
				},
			},
		},
		{
			Name: volumeData,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: dataPVCName(instanceName),
				},
			},
		},
		{
			// Odoo needs a writable /tmp because the root filesystem is read-only.
			Name: volumeTmp,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	}
}

// odooPodSpec is the pod spec shared by the web and cron pods. Only the container and the
// affinity differ between them.
func odooPodSpec(instance *odoov1.OdooInstance, container corev1.Container, affinity *corev1.Affinity) corev1.PodSpec {
	container.Image = odooImage(&instance.Spec)
	container.Env = buildOdooEnv(&instance.Spec)
	container.VolumeMounts = odooVolumeMounts()
	container.SecurityContext = odooContainerSecurityContext()
	container.TerminationMessagePath = corev1.TerminationMessagePathDefault
	container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	return corev1.PodSpec{
		AutomountServiceAccountToken:  boolPtr(false),
		TerminationGracePeriodSeconds: int64Ptr(terminationGraceSecs),
		RestartPolicy:                 corev1.RestartPolicyAlways,
		DNSPolicy:                     corev1.DNSClusterFirst,
		SchedulerName:                 corev1.DefaultSchedulerName,
		SecurityContext:               odooPodSecurityContext(),
		Affinity:                      affinity,
		Containers:                    []corev1.Container{container},
		Volumes:                       odooVolumes(instance.Name),
	}
}

// needsColocation reports whether the data or addons volume is not ReadWriteMany. Such a
// volume attaches to one node at a time, so every pod of the instance must share a node.
func needsColocation(spec *odoov1.OdooInstanceSpec) bool {
	return spec.Data.AccessMode != corev1.ReadWriteMany || spec.Addons.AccessMode != corev1.ReadWriteMany
}

// colocationAffinity is the pod affinity of the web and cron pod templates when a volume is not
// ReadWriteMany. Both templates carry the same required term: a pod must run on a node that
// already runs a pod labelled instance=<name>. The first pod of the instance matches its own
// labels, so the scheduler places it freely. Every later web or cron pod joins that node, so a
// rollout of one template can never land on a node the other pod does not share. It is nil for
// ReadWriteMany volumes, where any node is fine.
func colocationAffinity(instanceName string, spec *odoov1.OdooInstanceSpec) *corev1.Affinity {
	if !needsColocation(spec) {
		return nil
	}
	return &corev1.Affinity{
		PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{labelInstance: instanceName}},
				TopologyKey:   corev1.LabelHostname,
			}},
		},
	}
}

// buildPodTemplate renders the full web pod template. Labels, annotations and the
// security posture are all set here so the Deployment is fully described by
// the instance spec and the rollout hashes.
func buildPodTemplate(instance *odoov1.OdooInstance, hashes rolloutHashes) corev1.PodTemplateSpec {
	startup, readiness, liveness := odooProbes()
	container := corev1.Container{
		Name:      appOdoo,
		Args:      buildOdooArgs(instance),
		Resources: odooResources(&instance.Spec),
		Ports: []corev1.ContainerPort{
			{Name: portNameOdoo, ContainerPort: portOdoo, Protocol: corev1.ProtocolTCP},
			{Name: portNameLongpolling, ContainerPort: portLongpolling, Protocol: corev1.ProtocolTCP},
		},
		StartupProbe:   startup,
		ReadinessProbe: readiness,
		LivenessProbe:  liveness,
	}
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      podLabels(instance.Name),
			Annotations: podAnnotations(hashes),
		},
		Spec: odooPodSpec(instance, container, colocationAffinity(instance.Name, &instance.Spec)),
	}
}
