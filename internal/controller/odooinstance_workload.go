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
	args := []string{"--config", odooConfPath}
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

// buildPodTemplate renders the full pod template. Labels, annotations and the
// security posture are all set here so the Deployment is fully described by
// the instance spec and the rollout hashes.
func buildPodTemplate(instance *odoov1.OdooInstance, hashes rolloutHashes) corev1.PodTemplateSpec {
	startup, readiness, liveness := odooProbes()
	readOnly := true

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels: podLabels(instance.Name),
			Annotations: map[string]string{
				annotationConfigHash:  hashes.config,
				annotationSecretsHash: hashes.secrets,
				annotationAddonsHash:  hashes.addons,
			},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken:  boolPtr(false),
			TerminationGracePeriodSeconds: int64Ptr(terminationGraceSecs),
			RestartPolicy:                 corev1.RestartPolicyAlways,
			DNSPolicy:                     corev1.DNSClusterFirst,
			SchedulerName:                 corev1.DefaultSchedulerName,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: boolPtr(true),
				RunAsUser:    int64Ptr(odooUID),
				FSGroup:      int64Ptr(odooUID),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{
				{
					Name:      appOdoo,
					Image:     odooImage(&instance.Spec),
					Args:      buildOdooArgs(instance),
					Env:       buildOdooEnv(&instance.Spec),
					Resources: odooResources(&instance.Spec),
					Ports: []corev1.ContainerPort{
						{Name: portNameOdoo, ContainerPort: portOdoo, Protocol: corev1.ProtocolTCP},
						{Name: portNameLongpolling, ContainerPort: portLongpolling, Protocol: corev1.ProtocolTCP},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: volumeConfig, MountPath: mountPathConfig, ReadOnly: true},
						{Name: volumeAddons, MountPath: addonsMountPath, ReadOnly: true},
						{Name: volumeData, MountPath: mountPathData},
						{Name: volumeTmp, MountPath: mountPathTmp},
					},
					StartupProbe:   startup,
					ReadinessProbe: readiness,
					LivenessProbe:  liveness,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: boolPtr(false),
						ReadOnlyRootFilesystem:   &readOnly,
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
					TerminationMessagePath:   corev1.TerminationMessagePathDefault,
					TerminationMessagePolicy: corev1.TerminationMessageReadFile,
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: volumeConfig,
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: configMapName(instance.Name)},
							DefaultMode:          int32Ptr(0o644),
						},
					},
				},
				{
					Name: volumeAddons,
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: addonsPVCName(instance.Name),
							ReadOnly:  true,
						},
					},
				},
				{
					Name: volumeData,
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: dataPVCName(instance.Name),
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
			},
		},
	}
}
