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
	stderrors "errors"
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	odoov1 "github.com/odoo-k8s/odoo-k8-operator/api/v1"
)

const (
	hardeningConflict = "hardening-conflict"

	conflictMessage  = "the object has been modified; please apply your changes to the latest version and try again"
	conflictWrapText = "update status"
	notFoundText     = "not here"
	genericErrText   = "boom"
)

func newConflict() error {
	return errors.NewConflict(schema.GroupResource{Group: odoov1.GroupVersion.Group, Resource: "odooinstances"},
		"example", fmt.Errorf("%s", conflictMessage))
}

// TestIsConflict is the table for the classification helper.
func TestIsConflict(t *testing.T) {
	conflict := newConflict()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "conflict", err: conflict, want: true},
		{name: "conflict wrapped once", err: fmt.Errorf("%s: %w", conflictWrapText, conflict), want: true},
		{name: "conflict wrapped twice", err: fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", conflict)), want: true},
		{name: "conflict wrapped as reconcile reason", err: withReason(odoov1.ReasonReconcileError, conflict), want: true},
		{name: "not found", err: errors.NewNotFound(schema.GroupResource{Resource: "odooinstances"}, "example"), want: false},
		{name: "wrapped not found", err: fmt.Errorf("%s: %w", notFoundText,
			errors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "x")), want: false},
		{name: "generic error", err: fmt.Errorf("%s", genericErrText), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isConflict(tc.err); got != tc.want {
				t.Errorf("isConflict(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestConflictResult checks that a conflict becomes a quiet requeue and other errors pass through.
func TestConflictResult(t *testing.T) {
	res, err := conflictResult(fmt.Errorf("wrapped: %w", newConflict()))
	if err != nil {
		t.Errorf("conflictResult(conflict) error = %v, want nil", err)
	}
	if res.RequeueAfter != conflictRequeue {
		t.Errorf("conflictResult(conflict) RequeueAfter = %v, want %v", res.RequeueAfter, conflictRequeue)
	}

	generic := fmt.Errorf("%s", genericErrText)
	res, err = conflictResult(generic)
	if !stderrors.Is(err, generic) {
		t.Errorf("conflictResult(generic) error = %v, want %v", err, generic)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("conflictResult(generic) result = %+v, want zero", res)
	}
}

// conflictStatusClient fails every status update with a 409, the way a stale write does
// when another writer changed the object first.
type conflictStatusClient struct {
	client.Client
}

func (c conflictStatusClient) Status() client.SubResourceWriter {
	return conflictStatusWriter{SubResourceWriter: c.Client.Status()}
}

type conflictStatusWriter struct {
	client.SubResourceWriter
}

func (conflictStatusWriter) Update(_ context.Context, obj client.Object, _ ...client.SubResourceUpdateOption) error {
	return fmt.Errorf("update status of %s: %w", obj.GetName(), newConflict())
}

var _ = Describe("OdooInstance update conflicts", func() {
	It("requeues without an error, a Degraded condition or a ReconcileFailed event", func() {
		ctx := context.Background()
		createInstance(ctx, hardeningConflict, nil)

		rec := events.NewFakeRecorder(50)
		reconciler := &OdooInstanceReconciler{
			Client:   conflictStatusClient{Client: k8sClient},
			Scheme:   k8sClient.Scheme(),
			Recorder: rec,
		}
		result, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: client.ObjectKey{Name: hardeningConflict, Namespace: testNamespace},
		})

		Expect(err).NotTo(HaveOccurred(), "a conflict must not be returned as a reconcile error")
		Expect(result.RequeueAfter).To(Equal(conflictRequeue))
		Expect(cxDrain(rec)).To(BeEmpty(), "a conflict must not emit events")

		instance := fetchInstance(ctx, hardeningConflict)
		Expect(meta.FindStatusCondition(instance.Status.Conditions, odoov1.ConditionTypeDegraded)).To(BeNil(),
			"a conflict must not set Degraded")
		Expect(instance.Status.Phase).To(BeEmpty(), "a conflict must not change the phase")
	})
})
