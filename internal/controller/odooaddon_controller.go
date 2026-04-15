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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	finalizerOdooAddon = "odoo.operator.io/addon-finalizer"
	cloneMountPath     = "/mnt/addons-clone"

	phaseFailed  = "Failed"
	phasePending = "Pending"
	phaseCloning = "Cloning"
	phaseSynced  = "Synced"
)

var addonLogger = logf.Log.WithName("controller_odooaddon")

type OdooAddonReconciler struct {
	client.Client
	Scheme *runtime.Scheme
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

	result, err := r.reconcileOdooAddon(ctx, addon)
	if err != nil {
		addonLogger.Error(err, "Failed to reconcile OdooAddon")
		addon.Status.Phase = phaseFailed
		addon.Status.Ready = false
		if statusErr := r.Status().Update(ctx, addon); statusErr != nil {
			addonLogger.Error(statusErr, "Failed to update addon status")
		}
	}

	return result, err
}

func (r *OdooAddonReconciler) reconcileOdooAddon(ctx context.Context, addon *odoov1.OdooAddon) (ctrl.Result, error) {
	instanceName := addon.Spec.InstanceRef.Name
	instanceNamespace := addon.Namespace
	if addon.Spec.InstanceRef.Namespace != "" && addon.Spec.InstanceRef.Namespace != addon.Namespace {
		addon.Status.Phase = phaseFailed
		addon.Status.Ready = false
		_ = r.Status().Update(ctx, addon)
		return ctrl.Result{}, nil
	}

	if addon.Spec.InstanceRef.Name == "" {
		addon.Status.Phase = phasePending
		addon.Status.Ready = false
		if statusErr := r.Status().Update(ctx, addon); statusErr != nil {
			addonLogger.Error(statusErr, "Failed to update addon status")
		}
		return ctrl.Result{}, nil
	}

	instance := &odoov1.OdooInstance{}
	if err := r.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: instanceNamespace}, instance); err != nil {
		if errors.IsNotFound(err) {
			addon.Status.Phase = phasePending
			addon.Status.Ready = false
			if statusErr := r.Status().Update(ctx, addon); statusErr != nil {
				addonLogger.Error(statusErr, "Failed to update addon status")
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	addon.Status.Phase = phaseCloning

	gitUrl := addon.Spec.GitUrl
	gitRef := addon.Spec.GitRef
	if gitRef == "" {
		gitRef = "main"
	}

	addonPath := addon.Spec.AddonPath
	singleAddon := addon.Spec.SingleAddon

	pvcName := fmt.Sprintf("%s-addons", instance.Name)
	cloneDir := filepath.Join(cloneMountPath, addon.Name)

	volumeMounted, err := r.ensureVolumeMounted(ctx, instance, pvcName)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !volumeMounted {
		addon.Status.Phase = phasePending
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	commitHash, err := r.cloneOrUpdateRepo(gitUrl, gitRef, cloneDir, addonPath, singleAddon)
	if err != nil {
		addonLogger.Error(err, "Failed to clone/update repository")
		addon.Status.Phase = phaseFailed
		addon.Status.Ready = false
		if statusErr := r.Status().Update(ctx, addon); statusErr != nil {
			addonLogger.Error(statusErr, "Failed to update addon status")
		}
		return ctrl.Result{}, err
	}

	addon.Status.ClonedCommit = commitHash
	now := metav1.Now()
	addon.Status.LastSyncTime = &now
	addon.Status.Phase = phaseSynced
	addon.Status.Ready = true

	if err := r.Status().Update(ctx, addon); err != nil {
		return ctrl.Result{}, err
	}

	addonPaths := instance.Status.AddonPaths
	newPath := cloneDir
	if !singleAddon && addonPath != "" {
		newPath = filepath.Join(cloneDir, addonPath)
	}

	found := false
	for _, p := range addonPaths {
		if p == newPath {
			found = true
			break
		}
	}
	if !found {
		addonPaths = append(addonPaths, newPath)
		instance.Status.AddonPaths = addonPaths
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

var validGitRef = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)

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

func (r *OdooAddonReconciler) ensureVolumeMounted(ctx context.Context, instance *odoov1.OdooInstance, pvcName string) (bool, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: instance.Namespace}, pvc); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	if pvc.Status.Phase != corev1.ClaimBound {
		return false, nil
	}

	return true, nil
}

func (r *OdooAddonReconciler) cloneOrUpdateRepo(gitUrl, gitRef, cloneDir, addonPath string, singleAddon bool) (string, error) {
	if err := validateGitUrl(gitUrl); err != nil {
		return "", err
	}
	if err := validateGitRef(gitRef); err != nil {
		return "", err
	}

	repoDir := cloneDir
	if !singleAddon && addonPath != "" {
		repoDir = filepath.Join(cloneDir, addonPath)
	}

	gitDir := filepath.Join(repoDir, ".git")

	if _, err := os.Stat(gitDir); os.IsNotExist(err) {
		addonLogger.Info("Cloning repository", "url", gitUrl, "ref", gitRef, "dir", repoDir)

		if err := os.MkdirAll(filepath.Dir(repoDir), 0755); err != nil {
			return "", fmt.Errorf("failed to create directory: %w", err)
		}

		cmd := exec.Command("git", "clone", "--depth", "1", "--branch", gitRef, gitUrl, repoDir)
		cmd.Dir = filepath.Dir(repoDir)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to clone repository: %w, output: %s", err, string(output))
		}
	} else {
		addonLogger.Info("Updating repository", "url", gitUrl, "ref", gitRef, "dir", repoDir)

		cmd := exec.Command("git", "fetch", "origin")
		cmd.Dir = repoDir
		if output, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git fetch failed: %w, output: %s", err, string(output))
		}

		cmd = exec.Command("git", "checkout", gitRef)
		cmd.Dir = repoDir
		if output, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to checkout ref: %w, output: %s", err, string(output))
		}

		cmd = exec.Command("git", "pull", "origin", gitRef)
		cmd.Dir = repoDir
		if output, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git pull failed: %w, output: %s", err, string(output))
		}
	}

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repoDir
	commitHash, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get commit hash: %w", err)
	}

	return strings.TrimSpace(string(commitHash)), nil
}

func (r *OdooAddonReconciler) handleFinalizer(ctx context.Context, addon *odoov1.OdooAddon) error {
	if controllerutil.ContainsFinalizer(addon, finalizerOdooAddon) {
		if addon.Spec.InstanceRef.Name != "" {
			instance := &odoov1.OdooInstance{}
			nsName := types.NamespacedName{Name: addon.Spec.InstanceRef.Name, Namespace: addon.Namespace}
			if err := r.Get(ctx, nsName, instance); err == nil {
				targetPath := filepath.Join(cloneMountPath, addon.Name)
				updated := instance.Status.AddonPaths[:0]
				for _, p := range instance.Status.AddonPaths {
					if p != targetPath {
						updated = append(updated, p)
					}
				}
				instance.Status.AddonPaths = updated
				_ = r.Status().Update(ctx, instance)
			}
		}
		controllerutil.RemoveFinalizer(addon, finalizerOdooAddon)
		return r.Update(ctx, addon)
	}
	return nil
}

func (r *OdooAddonReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&odoov1.OdooAddon{}).
		Named("odooaddon").
		Complete(r)
}
