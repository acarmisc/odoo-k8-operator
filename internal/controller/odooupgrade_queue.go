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
	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

// upgradeQueueDecision says whether a Pending upgrade may start its Job now.
type upgradeQueueDecision struct {
	MayStart bool
	// BlockedBy names the upgrade holding the instance (Running) or queued ahead of
	// it (oldest Pending). Empty when MayStart is true.
	BlockedBy string
}

// upgradeDecideStart serializes upgrades per instance. all is the list of OdooUpgrades
// in the candidate's namespace; only those naming the same instance matter. The
// candidate may start only when no sibling is Running and no sibling Pending upgrade
// is older (creationTimestamp, then name). Terminal siblings never block.
func upgradeDecideStart(candidate *odoov1.OdooUpgrade, all []odoov1.OdooUpgrade) upgradeQueueDecision {
	instance := candidate.Spec.InstanceRef.Name
	for i := range all {
		other := &all[i]
		if other.Name == candidate.Name || other.Spec.InstanceRef.Name != instance {
			continue
		}
		switch other.Status.Phase {
		case odoov1.UpgradePhaseRunning:
			return upgradeQueueDecision{BlockedBy: other.Name}
		case "", odoov1.UpgradePhasePending:
			if upgradeOlder(other, candidate) {
				return upgradeQueueDecision{BlockedBy: other.Name}
			}
		}
	}
	return upgradeQueueDecision{MayStart: true}
}

// upgradeOlder orders upgrades by creation time, breaking ties by name.
func upgradeOlder(a, b *odoov1.OdooUpgrade) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}
