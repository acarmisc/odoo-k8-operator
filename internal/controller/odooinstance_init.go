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

// One-time database initialisation (spec.initialize). The OdooUpgrade "<name>-init" runs
// `odoo -i <modules> --stop-after-init` and sets the admin password. Until it succeeds the
// Deployment stays at zero replicas, so no Odoo pod runs against an empty database. The
// decision (initDecide) is pure; decideInitialization reads the cluster and applies it.

import (
	"context"
	"fmt"
	"slices"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	phaseInitializing = "Initializing"
	phaseInitFailed   = "InitializationFailed"
	initUpgradeSuffix = "-init"
	// initFailedRequeue is how often a failed initialisation is looked at again. Retrying
	// is manual (delete the failed OdooUpgrade), so the requeue only keeps status fresh.
	initFailedRequeue = 60 * time.Second
	// initMsgFailedDefault is the Degraded message when the upgrade failed without a message.
	initMsgFailedDefault = "initialisation failed"
)

// initInput is everything the initialisation decision depends on.
type initInput struct {
	// requested is true when spec.initialize is set.
	requested bool
	// initialized is status.initialized.
	initialized bool
	// upgradeName is the expected name of the initialisation OdooUpgrade.
	upgradeName string
	// upgradeExists, upgradePhase and upgradeMessage describe that OdooUpgrade if it exists.
	upgradeExists  bool
	upgradePhase   string
	upgradeMessage string
	// liveReplicas is the ready-pod count of an existing Deployment, 0 if there is none.
	liveReplicas int32
}

// initDecision is what the reconciler applies in this pass.
type initDecision struct {
	// pending keeps the Deployment at zero replicas and skips the addons upgrade gate.
	pending bool
	// initialized is the value for status.initialized.
	initialized bool
	// ensureUpgrade asks the reconciler to create the initialisation OdooUpgrade.
	ensureUpgrade bool
	// upgradeName is the value for status.initUpgrade ("" clears it).
	upgradeName string
	// phase overrides status.phase when pending: Initializing or InitializationFailed.
	phase string
	// failMessage is the failed upgrade's message, for the Degraded condition.
	failMessage string
	// succeeded names the initialisation OdooUpgrade that completed in this pass.
	succeeded string
	// adopted is set when the database was treated as initialised without running anything.
	adopted bool
}

// initDecide maps the state to the initialisation decision. It has no side effects.
func initDecide(in initInput) initDecision {
	if !in.requested || in.initialized {
		return initDecision{initialized: in.initialized}
	}
	// A Deployment with ready pods and no initialisation upgrade means the database already
	// serves traffic (for example after the status was lost). Scaling to zero would be an
	// outage and re-running -i against a live database is unsafe, so the status is set
	// and nothing runs.
	if !in.upgradeExists && in.liveReplicas > 0 {
		return initDecision{initialized: true, adopted: true}
	}
	if !in.upgradeExists {
		return initDecision{pending: true, ensureUpgrade: true, upgradeName: in.upgradeName, phase: phaseInitializing}
	}
	switch in.upgradePhase {
	case odoov1.UpgradePhaseSucceeded:
		return initDecision{initialized: true, succeeded: in.upgradeName}
	case odoov1.UpgradePhaseFailed:
		return initDecision{pending: true, upgradeName: in.upgradeName, phase: phaseInitFailed, failMessage: in.upgradeMessage}
	default:
		return initDecision{pending: true, upgradeName: in.upgradeName, phase: phaseInitializing}
	}
}

// initUpgradeName returns "<instance>-init".
func initUpgradeName(instanceName string) string {
	return instanceName + initUpgradeSuffix
}

// initHoldGate is the addons decision while the database is being initialised. No pod runs
// yet, so there is nothing to hold back; the current revision is recorded as applied so the
// first revision is never gated.
func initHoldGate(instance *odoov1.OdooInstance, current string) gateDecision {
	instance.Status.ActiveUpgrade = ""
	instance.Status.AppliedAddonsHash = current
	return gateDecision{templateHash: current, appliedHash: current}
}

// decideInitialization reads the cluster, applies initDecide and creates the initialisation
// OdooUpgrade when it is needed.
func (r *OdooInstanceReconciler) decideInitialization(ctx context.Context, instance *odoov1.OdooInstance) (initDecision, error) {
	in := initInput{
		requested:   instance.Spec.Initialize != nil,
		initialized: instance.Status.Initialized,
		upgradeName: initUpgradeName(instance.Name),
	}
	if in.requested && !in.initialized {
		if err := r.readInitState(ctx, instance, &in); err != nil {
			return initDecision{}, err
		}
	}
	dec := initDecide(in)
	if dec.ensureUpgrade {
		if err := r.checkInitSecret(ctx, instance); err != nil {
			return initDecision{}, err
		}
		if err := r.createInitUpgrade(ctx, instance, dec.upgradeName); err != nil {
			return initDecision{}, err
		}
	}
	return dec, nil
}

// readInitState fills the cluster facts of initInput.
func (r *OdooInstanceReconciler) readInitState(ctx context.Context, instance *odoov1.OdooInstance, in *initInput) error {
	deploy := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: instance.Name, Namespace: instance.Namespace}, deploy)
	switch {
	case err == nil:
		in.liveReplicas = deploy.Status.ReadyReplicas
	case !errors.IsNotFound(err):
		return fmt.Errorf("get Deployment %q: %w", instance.Name, err)
	}

	upgrade := &odoov1.OdooUpgrade{}
	key := types.NamespacedName{Name: in.upgradeName, Namespace: instance.Namespace}
	err = r.Get(ctx, key, upgrade)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get OdooUpgrade %q: %w", in.upgradeName, err)
	}
	if upgrade.Spec.InstanceRef.Name != instance.Name {
		return fmt.Errorf("OdooUpgrade %q belongs to instance %q, not %q",
			in.upgradeName, upgrade.Spec.InstanceRef.Name, instance.Name)
	}
	in.upgradeExists = true
	in.upgradePhase = upgrade.Status.Phase
	in.upgradeMessage = upgrade.Status.Message
	return nil
}

// checkInitSecret fails before the Job is created when the password Secret is missing. A
// Job whose pod cannot mount the Secret would only time out.
func (r *OdooInstanceReconciler) checkInitSecret(ctx context.Context, instance *odoov1.OdooInstance) error {
	name := instance.Spec.Initialize.AdminUserPasswordSecret
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: instance.Namespace}, &corev1.Secret{})
	if errors.IsNotFound(err) {
		return withReason(odoov1.ReasonMissingSecret,
			fmt.Errorf("secret %q referenced by spec.initialize.adminUserPasswordSecret does not exist in namespace %q",
				name, instance.Namespace))
	}
	if err != nil {
		return fmt.Errorf("get secret %q: %w", name, err)
	}
	return nil
}

// createInitUpgrade creates the initialisation OdooUpgrade, owned by the instance. An
// existing object with the same name is fine.
func (r *OdooInstanceReconciler) createInitUpgrade(ctx context.Context, instance *odoov1.OdooInstance, name string) error {
	spec := instance.Spec.Initialize
	upgrade := &odoov1.OdooUpgrade{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels:    map[string]string{gateLabelInstance: instance.Name},
		},
		Spec: odoov1.OdooUpgradeSpec{
			InstanceRef:             odoov1.OdooInstanceRef{Name: instance.Name},
			InstallModules:          slices.Clone(spec.Modules),
			WithDemo:                spec.WithDemo,
			AdminUserPasswordSecret: spec.AdminUserPasswordSecret,
			TimeoutSeconds:          gateCopyInt64Ptr(spec.TimeoutSeconds),
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

// initEvents lists the Events for an initialisation decision. It is called only after the
// status write succeeded, so each event is emitted once.
func initEvents(dec initDecision) []statusEvent {
	var evs []statusEvent
	if dec.succeeded != "" {
		evs = append(evs, statusEvent{
			Type: corev1.EventTypeNormal, Reason: eventReasonInitialized, Action: eventActionInit,
			Message:        fmt.Sprintf("database initialised by OdooUpgrade %q; rolling out the pods", dec.succeeded),
			RelatedUpgrade: dec.succeeded,
		})
	}
	if dec.adopted {
		evs = append(evs, statusEvent{
			Type: corev1.EventTypeWarning, Reason: eventReasonInitSkipped, Action: eventActionInit,
			Message: "spec.initialize is set but the Deployment already has ready pods; the database is " +
				"treated as initialised and nothing was run against it",
		})
	}
	return evs
}
