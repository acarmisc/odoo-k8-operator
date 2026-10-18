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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

// dataRetainOnDelete reports whether the data PVC (filestore) survives deletion
// of the OdooInstance. It defaults to true.
func dataRetainOnDelete(spec *odoov1.OdooInstanceSpec) bool {
	return spec.Data.RetainOnDelete == nil || *spec.Data.RetainOnDelete
}

// accessModesOrDefault returns the requested access mode, or ReadWriteOnce.
func accessModesOrDefault(mode corev1.PersistentVolumeAccessMode) []corev1.PersistentVolumeAccessMode {
	if mode == "" {
		mode = corev1.ReadWriteOnce
	}
	return []corev1.PersistentVolumeAccessMode{mode}
}

// hasOwnerRef reports whether any owner reference points at uid.
func hasOwnerRef(refs []metav1.OwnerReference, uid types.UID) bool {
	for i := range refs {
		if refs[i].UID == uid {
			return true
		}
	}
	return false
}

// ownerRefsWithoutUID drops every owner reference that points at uid.
func ownerRefsWithoutUID(refs []metav1.OwnerReference, uid types.UID) []metav1.OwnerReference {
	kept := make([]metav1.OwnerReference, 0, len(refs))
	for i := range refs {
		if refs[i].UID != uid {
			kept = append(kept, refs[i])
		}
	}
	return kept
}

func addonsPVCName(instanceName string) string {
	return instanceName + suffixAddonsPVC
}

func dataPVCName(instanceName string) string {
	return instanceName + suffixDataPVC
}

// pvcRequest describes one PVC the instance needs.
type pvcRequest struct {
	name        string
	size        string
	defaultSize string
	storage     *string
	accessMode  corev1.PersistentVolumeAccessMode
	// owned is true when the PVC must be garbage-collected with the instance.
	owned bool
}

func (r *OdooInstanceReconciler) reconcilePVC(ctx context.Context, instance *odoov1.OdooInstance) error {
	addons := pvcRequest{
		name:        addonsPVCName(instance.Name),
		size:        instance.Spec.Addons.Size,
		defaultSize: defaultAddonsSize,
		storage:     instance.Spec.Addons.StorageClass,
		accessMode:  instance.Spec.Addons.AccessMode,
		owned:       true,
	}
	data := pvcRequest{
		name:        dataPVCName(instance.Name),
		size:        instance.Spec.Data.Size,
		defaultSize: defaultDataSize,
		storage:     instance.Spec.Data.StorageClass,
		accessMode:  instance.Spec.Data.AccessMode,
		owned:       !dataRetainOnDelete(&instance.Spec),
	}
	if err := r.reconcileSinglePVC(ctx, instance, addons); err != nil {
		return err
	}
	return r.reconcileSinglePVC(ctx, instance, data)
}

func (r *OdooInstanceReconciler) reconcileSinglePVC(ctx context.Context, instance *odoov1.OdooInstance, req pvcRequest) error {
	pvc := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: req.name, Namespace: instance.Namespace}, pvc)
	if err == nil {
		// Spec is immutable once bound; only ownership is reconciled.
		return r.syncPVCOwnership(ctx, instance, pvc, req.owned)
	}
	if !errors.IsNotFound(err) {
		return err
	}

	size := req.size
	if size == "" {
		size = req.defaultSize
	}
	quantity, err := resource.ParseQuantity(size)
	if err != nil {
		return withReason(odoov1.ReasonInvalidConfig, fmt.Errorf("invalid size %q for PVC %s: %w", size, req.name, err))
	}

	newPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.name,
			Namespace: instance.Namespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: accessModesOrDefault(req.accessMode),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: quantity,
				},
			},
			// nil means the cluster default storage class.
			StorageClassName: req.storage,
		},
	}
	if req.owned {
		if err := controllerutil.SetControllerReference(instance, newPVC, r.Scheme); err != nil {
			return err
		}
	}
	return r.Create(ctx, newPVC)
}

// syncPVCOwnership makes the instance's owner reference on an existing PVC
// match the retention policy. Removing it is what keeps a retained filestore
// alive when the instance is deleted, including for PVCs created by earlier
// operator versions that always owned the data volume.
func (r *OdooInstanceReconciler) syncPVCOwnership(ctx context.Context, instance *odoov1.OdooInstance, pvc *corev1.PersistentVolumeClaim, owned bool) error {
	if owned == hasOwnerRef(pvc.OwnerReferences, instance.UID) {
		return nil
	}

	original := pvc.DeepCopy()
	if owned {
		if err := controllerutil.SetControllerReference(instance, pvc, r.Scheme); err != nil {
			return err
		}
	} else {
		pvc.OwnerReferences = ownerRefsWithoutUID(pvc.OwnerReferences, instance.UID)
	}
	return r.Patch(ctx, pvc, client.MergeFrom(original))
}
