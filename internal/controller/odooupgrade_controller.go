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

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

// OdooUpgradeReconciler runs one-shot `odoo -u/-i ... --stop-after-init` Jobs against an
// OdooInstance's database. TODO: implementation.
type OdooUpgradeReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clientset reads Job pod logs.
	Clientset kubernetes.Interface
}

func (r *OdooUpgradeReconciler) Reconcile(_ context.Context, _ ctrl.Request) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

func (r *OdooUpgradeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&odoov1.OdooUpgrade{}).
		Named("odooupgrade").
		Complete(r)
}
