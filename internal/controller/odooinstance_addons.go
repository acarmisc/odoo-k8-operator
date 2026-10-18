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
	"path/filepath"
	"slices"
	"strings"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

// filterReadyAddons keeps the ready OdooAddons bound to the named instance.
func filterReadyAddons(all []odoov1.OdooAddon, instanceName string) []odoov1.OdooAddon {
	ready := make([]odoov1.OdooAddon, 0, len(all))
	for i := range all {
		if all[i].Spec.InstanceRef.Name == instanceName && all[i].Status.Ready {
			ready = append(ready, all[i])
		}
	}
	return ready
}

// addonMountPaths returns the sorted paths the modules are mounted at inside the pod.
// Every synced module lands directly under addonsMountPath (see
// odooaddon_controller.go). An addon that reported its modules contributes one path per
// module. An addon that has not (synced before modules were tracked) falls back to its
// singleAddon folder, moduleName, which defaults to the addon's own name.
func addonMountPaths(addons []odoov1.OdooAddon) []string {
	paths := make([]string, 0, len(addons))
	for i := range addons {
		for _, module := range addonModules(&addons[i]) {
			paths = append(paths, filepath.Join(addonsMountPath, module))
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// addonModules returns the module folder names an addon provides on the addons volume.
func addonModules(addon *odoov1.OdooAddon) []string {
	if len(addon.Status.Modules) > 0 {
		return addon.Status.Modules
	}
	if addon.Spec.ModuleName != "" {
		return []string{addon.Spec.ModuleName}
	}
	return []string{addon.Name}
}

// addonRevisions returns sorted "addonName=ClonedCommit" entries. An addon that reported
// modules gets them appended, so a change in its module set changes the hash too. Addons
// without reported modules keep the plain entry.
func addonRevisions(addons []odoov1.OdooAddon) []string {
	revisions := make([]string, 0, len(addons))
	for i := range addons {
		rev := addons[i].Name + "=" + addons[i].Status.ClonedCommit
		if len(addons[i].Status.Modules) > 0 {
			modules := slices.Clone(addons[i].Status.Modules)
			slices.Sort(modules)
			rev += " modules=" + strings.Join(modules, ",")
		}
		revisions = append(revisions, rev)
	}
	slices.Sort(revisions)
	return revisions
}

// addonsHash fingerprints the exact source revision of every ready addon, so
// a re-sync to a new commit rolls the pods.
func addonsHash(addons []odoov1.OdooAddon) string {
	return sha256Hex(strings.Join(addonRevisions(addons), "\n"))
}
