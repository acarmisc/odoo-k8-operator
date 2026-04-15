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

package v1

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// +kubebuilder:webhook:path=/validate-odoo-operator-io-v1-odooaddon,mutating=false,failurePolicy=fail,sideEffects=None,groups=odoo.operator.io,resources=odooaddons,verbs=create;update,versions=v1,name=vodooaddon.kb.io,admissionReviewVersions=v1

// OdooAddonValidator validates OdooAddon resources at admission time.
type OdooAddonValidator struct{}

func (v *OdooAddonValidator) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&OdooAddon{}).
		WithValidator(v).
		Complete()
}

var _ admission.CustomValidator[*OdooAddon] = &OdooAddonValidator{}

var validAddonGitRef = regexp.MustCompile(`^[a-zA-Z0-9._/-]+$`)

func (v *OdooAddonValidator) ValidateCreate(ctx context.Context, obj *OdooAddon) (admission.Warnings, error) {
	return nil, validateOdooAddonSpec(obj)
}

func (v *OdooAddonValidator) ValidateUpdate(ctx context.Context, oldObj *OdooAddon, newObj *OdooAddon) (admission.Warnings, error) {
	return nil, validateOdooAddonSpec(newObj)
}

func (v *OdooAddonValidator) ValidateDelete(ctx context.Context, obj *OdooAddon) (admission.Warnings, error) {
	return nil, nil
}

func validateOdooAddonSpec(addon *OdooAddon) error {
	allowedSchemes := []string{"https://", "http://", "git@", "ssh://"}
	validURL := false
	for _, prefix := range allowedSchemes {
		if strings.HasPrefix(addon.Spec.GitUrl, prefix) {
			validURL = true
			break
		}
	}
	if !validURL {
		return fmt.Errorf("gitUrl scheme not allowed: %q; must start with one of: https://, http://, git@, ssh://", addon.Spec.GitUrl)
	}

	if addon.Spec.GitRef != "" {
		ref := addon.Spec.GitRef
		if strings.Contains(ref, "..") || strings.HasPrefix(ref, "-") {
			return fmt.Errorf("invalid gitRef %q: must not contain '..' or start with '-'", ref)
		}
		if !validAddonGitRef.MatchString(ref) {
			return fmt.Errorf("gitRef %q contains disallowed characters; only alphanumeric, '.', '_', '-', '/' allowed", ref)
		}
	}

	if addon.Spec.InstanceRef.Namespace != "" && addon.Spec.InstanceRef.Namespace != addon.Namespace {
		return fmt.Errorf("cross-namespace instanceRef is not permitted: instanceRef.namespace %q must equal addon namespace %q",
			addon.Spec.InstanceRef.Namespace, addon.Namespace)
	}

	return nil
}
