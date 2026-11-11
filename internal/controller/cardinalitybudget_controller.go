/*
Copyright 2026 Paul Greco.

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
	"errors"
	"fmt"
	"slices"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kardiganv1alpha1 "github.com/pwgreco/kardigan/api/v1alpha1"
	"github.com/pwgreco/kardigan/internal/budget"
	"github.com/pwgreco/kardigan/internal/mimir"
)

const (
	conditionReady        = "Ready"
	conditionWithinBudget = "WithinBudget"

	targetLabelNamespace = "namespace"

	defaultInterval = 5 * time.Minute
	retryInterval   = time.Minute
)

// CardinalityBudgetReconciler reconciles a CardinalityBudget object.
type CardinalityBudgetReconciler struct {
	client.Client
	Counter mimir.SeriesCounter
	// AdminNamespaces lists the namespaces whose budgets may target any
	// namespace. Budgets elsewhere may only target their own.
	AdminNamespaces []string
}

// +kubebuilder:rbac:groups=kardigan.paulgreco.ca,resources=cardinalitybudgets,verbs=get;list;watch
// +kubebuilder:rbac:groups=kardigan.paulgreco.ca,resources=cardinalitybudgets/status,verbs=update

// Reconcile counts the budget target's active series in Mimir, evaluates them
// against the budget, and records the result in status and metrics.
func (r *CardinalityBudgetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cb kardiganv1alpha1.CardinalityBudget
	if err := r.Get(ctx, req.NamespacedName, &cb); err != nil {
		if apierrors.IsNotFound(err) {
			deleteBudgetMetrics(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !targetAllowed(&cb, r.AdminNamespaces) {
		// Don't requeue: nothing changes until someone edits the spec or restarts
		// the operator with a different list, and both trigger a reconcile. Clear
		// what the budget reported before, which described a different target.
		deleteBudgetGauges(cb.Namespace, cb.Name)
		cb.Status.CurrentSeries = 0
		cb.Status.UsagePercent = 0
		cb.Status.LastChecked = nil
		cb.Status.ObservedGeneration = cb.Generation
		meta.RemoveStatusCondition(&cb.Status.Conditions, conditionWithinBudget)
		setCondition(&cb, conditionReady, metav1.ConditionFalse, "CrossNamespaceTarget",
			fmt.Sprintf("budgets in namespace %q may only target their own namespace, not %q",
				cb.Namespace, cb.Spec.Target.Value))
		return r.updateStatus(ctx, &cb, 0)
	}

	interval := cb.Spec.Interval.Duration
	if interval <= 0 {
		interval = defaultInterval
	}

	count, err := r.Counter.CountSeries(ctx, cb.Spec.Target.Label, cb.Spec.Target.Value)
	if err != nil {
		// A flaky Mimir is expected, so record it and retry on a fixed schedule.
		// Returning the error would mean exponential backoff and noisier logs.
		log.Error(err, "counting series", "label", cb.Spec.Target.Label, "value", cb.Spec.Target.Value)
		mimirQueryErrors.WithLabelValues(cb.Namespace, cb.Name).Inc()
		setCondition(&cb, conditionReady, metav1.ConditionFalse, "QueryFailed", queryFailureMessage(err))
		return r.updateStatus(ctx, &cb, retryInterval)
	}

	state, pct, err := budget.Evaluate(count, cb.Spec.MaxSeries, cb.Spec.WarnAtPercent)
	if err != nil {
		// The CRD's validation should make this unreachable. If it happens,
		// don't requeue: nothing changes until someone edits the spec.
		setCondition(&cb, conditionReady, metav1.ConditionFalse, "InvalidSpec", err.Error())
		return r.updateStatus(ctx, &cb, 0)
	}

	setBudgetGauges(&cb, count)

	now := metav1.Now()
	cb.Status.CurrentSeries = count
	cb.Status.UsagePercent = pct
	cb.Status.LastChecked = &now
	cb.Status.ObservedGeneration = cb.Generation

	within := metav1.ConditionTrue
	if state == budget.Exceeded {
		within = metav1.ConditionFalse
	}
	setCondition(&cb, conditionWithinBudget, within, string(state),
		fmt.Sprintf("%d of %d series (%d%%)", count, cb.Spec.MaxSeries, pct))
	setCondition(&cb, conditionReady, metav1.ConditionTrue, "QuerySucceeded", "")

	return r.updateStatus(ctx, &cb, interval)
}

// targetAllowed reports whether the budget may count its target. Only a namespace
// target can be checked, because a job label has no owning namespace.
func targetAllowed(cb *kardiganv1alpha1.CardinalityBudget, adminNamespaces []string) bool {
	return cb.Spec.Target.Label != targetLabelNamespace ||
		cb.Spec.Target.Value == cb.Namespace ||
		slices.Contains(adminNamespaces, cb.Namespace)
}

// queryFailureMessage describes a failed Mimir query for a resource's status.
// Whoever can read a budget might not be allowed to see Mimir's address or what it
// answered, so that detail goes only to the operator's log.
func queryFailureMessage(err error) string {
	if status, ok := errors.AsType[*mimir.StatusError](err); ok {
		return fmt.Sprintf("Mimir returned HTTP %d; the operator's log has the detail", status.Code)
	}
	return "Could not query Mimir; the operator's log has the detail"
}

// updateStatus writes status and schedules the next check. A conflict means
// someone changed the object since we read it; retry shortly with fresh data.
func (r *CardinalityBudgetReconciler) updateStatus(
	ctx context.Context, cb *kardiganv1alpha1.CardinalityBudget, next time.Duration,
) (ctrl.Result, error) {
	if err := r.Status().Update(ctx, cb); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: next}, nil
}

func setCondition(
	cb *kardiganv1alpha1.CardinalityBudget, condType string, status metav1.ConditionStatus, reason, message string,
) {
	meta.SetStatusCondition(&cb.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cb.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager. Status writes also
// count as updates, so without GenerationChangedPredicate each one would trigger
// another reconcile, and another Mimir query, instead of waiting for the interval.
func (r *CardinalityBudgetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kardiganv1alpha1.CardinalityBudget{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("cardinalitybudget").
		Complete(r)
}
