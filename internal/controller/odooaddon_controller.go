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
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

// Odoo discovers modules by scanning addons_path directories for immediate
// subfolders containing __manifest__.py - the config renders a single flat
// addons_path (see reconcileConfigMap), so every synced module must land
// directly under addonsMountPath, never nested under a repo/addon-name folder.
const (
	finalizerOdooAddon = "odoo.operator.io/addon-finalizer"
	addonsMountPath    = "/mnt/odoo/addons"

	phaseFailed  = "Failed"
	phasePending = "Pending"
	phaseCloning = "Cloning"
	phaseSynced  = "Synced"

	syncJobImage   = "alpine/git:2.45.2"
	resyncInterval = 5 * time.Minute
	jobWaitTimeout = 3 * time.Minute
)

var addonLogger = logf.Log.WithName("controller_odooaddon")

type OdooAddonReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clientset reads sync Job pod logs (client.Client has no log-streaming support).
	Clientset kubernetes.Interface
}

func (r *OdooAddonReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	addonLogger.Info("Reconciling OdooAddon", "request", req.Name)

	addon := &odoov1.OdooAddon{}
	err := r.Get(ctx, req.NamespacedName, addon)
	if err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		addonLogger.Error(err, "Failed to get OdooAddon")
		return ctrl.Result{}, err
	}

	if addon.DeletionTimestamp != nil {
		return ctrl.Result{}, r.handleFinalizer(ctx, addon)
	}

	if !controllerutil.ContainsFinalizer(addon, finalizerOdooAddon) {
		controllerutil.AddFinalizer(addon, finalizerOdooAddon)
		if err := r.Update(ctx, addon); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.reconcileOdooAddon(ctx, addon)
}

func (r *OdooAddonReconciler) reconcileOdooAddon(ctx context.Context, addon *odoov1.OdooAddon) (ctrl.Result, error) {
	instanceName := addon.Spec.InstanceRef.Name
	instanceNamespace := addon.Namespace
	if addon.Spec.InstanceRef.Namespace != "" && addon.Spec.InstanceRef.Namespace != addon.Namespace {
		return r.fail(ctx, addon, fmt.Errorf("cross-namespace instanceRef is not supported"))
	}

	if instanceName == "" {
		return r.pending(ctx, addon)
	}

	instance := &odoov1.OdooInstance{}
	if err := r.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: instanceNamespace}, instance); err != nil {
		if errors.IsNotFound(err) {
			return r.pending(ctx, addon)
		}
		return ctrl.Result{}, err
	}

	pvcName := fmt.Sprintf("%s-addons", instance.Name)
	pvcReady, err := r.ensureVolumeMounted(ctx, instance, pvcName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !pvcReady {
		return r.pending(ctx, addon)
	}

	// Resync periodically (picks up upstream git changes) instead of only once.
	if addon.Status.Phase == phaseSynced && addon.Status.LastSyncTime != nil {
		if elapsed := time.Since(addon.Status.LastSyncTime.Time); elapsed < resyncInterval {
			return ctrl.Result{RequeueAfter: resyncInterval - elapsed}, nil
		}
	}

	addon.Status.Phase = phaseCloning
	if err := r.Status().Update(ctx, addon); err != nil {
		return ctrl.Result{}, err
	}

	nodeName := r.odooPodNode(ctx, instance)

	commitHash, err := r.runSyncJob(ctx, addon, pvcName, nodeName)
	if err != nil {
		return r.fail(ctx, addon, err)
	}

	addon.Status.ClonedCommit = commitHash
	now := metav1.Now()
	addon.Status.LastSyncTime = &now
	addon.Status.Phase = phaseSynced
	addon.Status.Ready = true
	if err := r.Status().Update(ctx, addon); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

func (r *OdooAddonReconciler) fail(ctx context.Context, addon *odoov1.OdooAddon, cause error) (ctrl.Result, error) {
	addonLogger.Error(cause, "OdooAddon sync failed", "addon", addon.Name)
	addon.Status.Phase = phaseFailed
	addon.Status.Ready = false
	if statusErr := r.Status().Update(ctx, addon); statusErr != nil {
		addonLogger.Error(statusErr, "Failed to update addon status")
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, cause
}

func (r *OdooAddonReconciler) pending(ctx context.Context, addon *odoov1.OdooAddon) (ctrl.Result, error) {
	addon.Status.Phase = phasePending
	addon.Status.Ready = false
	if err := r.Status().Update(ctx, addon); err != nil {
		addonLogger.Error(err, "Failed to update addon status")
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

var validGitRef = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)
var validPathSegment = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func validateGitUrl(u string) error {
	allowed := []string{"https://", "http://", "git@", "ssh://"}
	for _, prefix := range allowed {
		if strings.HasPrefix(u, prefix) {
			return nil
		}
	}
	return fmt.Errorf("gitUrl scheme not allowed: %q", u)
}

func validateGitRef(ref string) error {
	if strings.Contains(ref, "..") || strings.HasPrefix(ref, "-") {
		return fmt.Errorf("invalid gitRef: %q", ref)
	}
	if !validGitRef.MatchString(ref) {
		return fmt.Errorf("gitRef contains disallowed characters: %q", ref)
	}
	return nil
}

// validatePathSegment guards moduleName/addonPath, which get interpolated into the
// sync Job's shell script - reject anything but a plain relative path/name so a
// malicious OdooAddon spec can't break out of /addons or inject shell commands.
func validatePathSegment(name string) error {
	if name == "" || strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		return fmt.Errorf("invalid path segment: %q", name)
	}
	if !validPathSegment.MatchString(name) {
		return fmt.Errorf("path segment contains disallowed characters: %q", name)
	}
	return nil
}

func (r *OdooAddonReconciler) ensureVolumeMounted(ctx context.Context, instance *odoov1.OdooInstance, pvcName string) (bool, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: instance.Namespace}, pvc); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return pvc.Status.Phase == corev1.ClaimBound, nil
}

// odooPodNode returns the node the instance's running Odoo pod is on, best-effort.
// The sync Job's pod is pinned to the same node so both it and the Odoo pod can mount
// the (ReadWriteOnce) addons PVC at once - RWO permits multiple pods on one node, but
// not across nodes. If no running pod is found yet, the scheduler picks freely.
func (r *OdooAddonReconciler) odooPodNode(ctx context.Context, instance *odoov1.OdooInstance) string {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(instance.Namespace), client.MatchingLabels{
		"app":      "odoo",
		"instance": instance.Name,
	}); err != nil {
		return ""
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning && p.Spec.NodeName != "" {
			return p.Spec.NodeName
		}
	}
	return ""
}

func (r *OdooAddonReconciler) runSyncJob(ctx context.Context, addon *odoov1.OdooAddon, pvcName, nodeName string) (string, error) {
	if err := validateGitUrl(addon.Spec.GitUrl); err != nil {
		return "", err
	}

	gitRef := addon.Spec.GitRef
	if gitRef == "" {
		gitRef = "main"
	}
	if err := validateGitRef(gitRef); err != nil {
		return "", err
	}

	moduleName := addon.Spec.ModuleName
	if moduleName == "" {
		moduleName = addon.Name
	}
	if err := validatePathSegment(moduleName); err != nil {
		return "", fmt.Errorf("invalid moduleName: %w", err)
	}

	addonPath := addon.Spec.AddonPath
	if addonPath != "" {
		if err := validatePathSegment(addonPath); err != nil {
			return "", fmt.Errorf("invalid addonPath: %w", err)
		}
	}

	jobName := fmt.Sprintf("%s-sync", addon.Name)
	if err := r.deleteJobIfExists(ctx, addon.Namespace, jobName); err != nil {
		return "", err
	}

	script := buildSyncScript(addon.Spec.SingleAddon, addonPath, moduleName)

	backoffLimit := int32(1)
	ttl := int32(300)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: addon.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "odoo-k8-operator",
				"odoo.operator.io/addon":       addon.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"odoo.operator.io/addon": addon.Name},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "sync",
							Image:   syncJobImage,
							Command: []string{"sh", "-c", script},
							Env: []corev1.EnvVar{
								{Name: "GIT_URL", Value: addon.Spec.GitUrl},
								{Name: "GIT_REF", Value: gitRef},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "addons", MountPath: addonsMountPath},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "addons",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
									ReadOnly:  false,
								},
							},
						},
					},
				},
			},
		},
	}

	if nodeName != "" {
		job.Spec.Template.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": nodeName}
	}

	if err := controllerutil.SetControllerReference(addon, job, r.Scheme); err != nil {
		return "", err
	}

	if err := r.Create(ctx, job); err != nil {
		return "", fmt.Errorf("failed to create sync job: %w", err)
	}

	return r.waitForJob(ctx, addon.Namespace, jobName)
}

// buildSyncScript clones into scratch space (/tmp/repo) first, never directly into the
// shared addons PVC - a partial clone left mid-sync would have Odoo read a broken module.
// Only after a full clone succeeds are the resolved module dirs copied into place.
func buildSyncScript(singleAddon bool, addonPath, moduleName string) string {
	if singleAddon {
		return fmt.Sprintf(`set -e
git clone --depth 1 --branch "$GIT_REF" "$GIT_URL" /tmp/repo
rm -rf %[1]q
cp -a /tmp/repo %[1]q
rm -rf %[1]q/.git
git -C /tmp/repo rev-parse HEAD
`, addonsMountPath+"/"+moduleName)
	}

	srcDir := "/tmp/repo"
	if addonPath != "" {
		srcDir = "/tmp/repo/" + addonPath
	}
	return fmt.Sprintf(`set -e
git clone --depth 1 --branch "$GIT_REF" "$GIT_URL" /tmp/repo
for mod in %[1]q/*/; do
  mod_name=$(basename "$mod")
  if [ -f "$mod/__manifest__.py" ]; then
    rm -rf %[2]q/"$mod_name"
    cp -a "$mod" %[2]q/"$mod_name"
  fi
done
git -C /tmp/repo rev-parse HEAD
`, srcDir, addonsMountPath)
}

func (r *OdooAddonReconciler) deleteJobIfExists(ctx context.Context, namespace, jobName string) error {
	existing := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: namespace}, existing)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	propagation := metav1.DeletePropagationForeground
	if err := r.Delete(ctx, existing, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !errors.IsNotFound(err) {
		return err
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: namespace}, &batchv1.Job{})
		if errors.IsNotFound(err) {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("timed out waiting for previous sync job %s to be deleted", jobName)
}

func (r *OdooAddonReconciler) waitForJob(ctx context.Context, namespace, jobName string) (string, error) {
	deadline := time.Now().Add(jobWaitTimeout)
	for time.Now().Before(deadline) {
		job := &batchv1.Job{}
		if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: namespace}, job); err != nil {
			return "", err
		}
		if job.Status.Succeeded > 0 {
			return r.readJobCommitHash(ctx, namespace, jobName)
		}
		if job.Status.Failed > 0 {
			logs, logErr := r.readJobLogs(ctx, namespace, jobName)
			if logErr != nil {
				return "", fmt.Errorf("sync job %s failed (could not read logs: %v)", jobName, logErr)
			}
			return "", fmt.Errorf("sync job %s failed: %s", jobName, strings.TrimSpace(logs))
		}
		time.Sleep(3 * time.Second)
	}
	return "", fmt.Errorf("sync job %s did not complete within %s", jobName, jobWaitTimeout)
}

func (r *OdooAddonReconciler) jobPodName(ctx context.Context, namespace, jobName string) (string, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"job-name": jobName}); err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pod found for job %s", jobName)
	}
	return pods.Items[0].Name, nil
}

func (r *OdooAddonReconciler) readJobLogs(ctx context.Context, namespace, jobName string) (string, error) {
	podName, err := r.jobPodName(ctx, namespace, jobName)
	if err != nil {
		return "", err
	}
	stream, err := r.Clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (r *OdooAddonReconciler) readJobCommitHash(ctx context.Context, namespace, jobName string) (string, error) {
	logs, err := r.readJobLogs(ctx, namespace, jobName)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if len(lines) == 0 || lines[len(lines)-1] == "" {
		return "", fmt.Errorf("sync job %s produced no commit hash output", jobName)
	}
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

func (r *OdooAddonReconciler) handleFinalizer(ctx context.Context, addon *odoov1.OdooAddon) error {
	if controllerutil.ContainsFinalizer(addon, finalizerOdooAddon) {
		_ = r.deleteJobIfExists(ctx, addon.Namespace, fmt.Sprintf("%s-sync", addon.Name))
		controllerutil.RemoveFinalizer(addon, finalizerOdooAddon)
		return r.Update(ctx, addon)
	}
	return nil
}

func (r *OdooAddonReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&odoov1.OdooAddon{}).
		Owns(&batchv1.Job{}).
		Named("odooaddon").
		Complete(r)
}
