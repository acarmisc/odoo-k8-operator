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
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

// The addons hash only advances once an OdooUpgrade for the new revision
// succeeded when spec.upgrade is set. Until then the pod template keeps the
// applied hash, so the running pods (built for the old code, against the
// upgraded-or-not database) are not replaced.
const (
	gatePhaseUpgrading     = "Upgrading"
	gatePhaseUpgradeFailed = "UpgradeFailed"

	gateLabelInstance = "odoo.operator.io/instance"
	gateUpgradeInfix  = "-upg-"
	gateHashPrefixLen = 8
	gateMaxNameLen    = 63
	gateFailedRequeue = 60 * time.Second
)

// gateInput is everything the rollout decision depends on. upgradeName,
// upgradeExists and upgradePhase are only meaningful when the addons hash
// differs from the applied one.
type gateInput struct {
	upgradeSet    bool
	applied       string
	current       string
	upgradeName   string
	upgradeExists bool
	upgradePhase  string
}

// gateDecision is what the reconciler writes back for this pass.
type gateDecision struct {
	// templateHash is the addons hash stamped on the pod template.
	templateHash string
	// appliedHash is the value for status.appliedAddonsHash.
	appliedHash string
	// activeUpgrade is the value for status.activeUpgrade ("" clears it).
	activeUpgrade string
	// phase overrides status.phase when non-empty.
	phase string
	// ensureUpgrade asks the reconciler to create the OdooUpgrade if it is missing.
	ensureUpgrade bool
}

// gateDecide maps the current state to a rollout decision. It has no side effects.
func gateDecide(in gateInput) gateDecision {
	// Without spec.upgrade, or on the first reconcile with the feature (a fresh
	// database has nothing to upgrade), roll to the current revision directly.
	if !in.upgradeSet || in.applied == "" {
		return gateDecision{templateHash: in.current, appliedHash: in.current}
	}
	// Already running the current revision: nothing pending.
	if in.current == in.applied {
		return gateDecision{templateHash: in.applied, appliedHash: in.applied}
	}

	// The revision changed: hold the pods on the applied revision until the upgrade succeeds.
	held := gateDecision{
		templateHash:  in.applied,
		appliedHash:   in.applied,
		activeUpgrade: in.upgradeName,
		phase:         gatePhaseUpgrading,
	}
	switch {
	case !in.upgradeExists:
		held.ensureUpgrade = true
	case in.upgradePhase == odoov1.UpgradePhaseSucceeded:
		return gateDecision{templateHash: in.current, appliedHash: in.current}
	case in.upgradePhase == odoov1.UpgradePhaseFailed:
		held.phase = gatePhaseUpgradeFailed
	}
	return held
}

// gateUpgradeName returns "<instance>-upg-<first 8 hex of hash>", truncating the
// instance part so the name stays within 63 characters.
func gateUpgradeName(instanceName, hash string) string {
	maxPrefix := gateMaxNameLen - len(gateUpgradeInfix) - gateHashPrefixLen
	prefix := instanceName[:min(len(instanceName), maxPrefix)]
	prefix = strings.TrimRight(prefix, "-")
	return prefix + gateUpgradeInfix + hash[:gateHashPrefixLen]
}

// gateAddonsRollout decides which addons hash the pods run and records it in the
// instance status. It creates the OdooUpgrade when one is needed.
func (r *OdooInstanceReconciler) gateAddonsRollout(ctx context.Context, instance *odoov1.OdooInstance, current string) (gateDecision, error) {
	in := gateInput{
		upgradeSet: instance.Spec.Upgrade != nil,
		applied:    instance.Status.AppliedAddonsHash,
		current:    current,
	}
	if in.upgradeSet && in.applied != "" && in.applied != current {
		in.upgradeName = gateUpgradeName(instance.Name, current)
		upgrade := &odoov1.OdooUpgrade{}
		key := types.NamespacedName{Name: in.upgradeName, Namespace: instance.Namespace}
		switch err := r.Get(ctx, key, upgrade); {
		case errors.IsNotFound(err):
			in.upgradeExists = false
		case err != nil:
			return gateDecision{}, fmt.Errorf("get OdooUpgrade %q: %w", in.upgradeName, err)
		case upgrade.Spec.InstanceRef.Name != instance.Name:
			// Truncated names of two instances with the same long prefix can collide.
			return gateDecision{}, fmt.Errorf("OdooUpgrade %q belongs to instance %q, not %q",
				in.upgradeName, upgrade.Spec.InstanceRef.Name, instance.Name)
		default:
			in.upgradeExists = true
			in.upgradePhase = upgrade.Status.Phase
		}
	}

	decision := gateDecide(in)
	if decision.ensureUpgrade {
		if err := r.gateCreateUpgrade(ctx, instance, in.upgradeName); err != nil {
			return gateDecision{}, err
		}
	}
	if decision.phase == gatePhaseUpgradeFailed {
		odooLogger.Info("OdooUpgrade failed; keeping the applied addons revision",
			"instance", instance.Name, "upgrade", in.upgradeName)
	}

	instance.Status.AppliedAddonsHash = decision.appliedHash
	instance.Status.ActiveUpgrade = decision.activeUpgrade
	return decision, nil
}

// gateCreateUpgrade creates the OdooUpgrade for the instance's spec.upgrade. An
// existing object with the same name is fine.
func (r *OdooInstanceReconciler) gateCreateUpgrade(ctx context.Context, instance *odoov1.OdooInstance, name string) error {
	upgrade := &odoov1.OdooUpgrade{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    map[string]string{gateLabelInstance: instance.Name},
		},
		Spec: odoov1.OdooUpgradeSpec{
			InstanceRef:    odoov1.OdooInstanceRef{Name: instance.Name},
			Modules:        append([]string(nil), instance.Spec.Upgrade.Modules...),
			TimeoutSeconds: gateCopyInt64Ptr(instance.Spec.Upgrade.TimeoutSeconds),
		},
	}
	if err := controllerutil.SetControllerReference(instance, upgrade, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, upgrade); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("create OdooUpgrade %q: %w", name, err)
	}
	return nil
}

func gateCopyInt64Ptr(p *int64) *int64 {
	if p == nil {
		return nil
	}
	return int64Ptr(*p)
}
