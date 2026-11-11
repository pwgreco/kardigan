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
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kardiganv1alpha1 "github.com/pwgreco/kardigan/api/v1alpha1"
	"github.com/pwgreco/kardigan/internal/recommend"
	"github.com/pwgreco/kardigan/internal/usage"
)

const (
	conditionUsageComplete = "UsageComplete"

	// reportRetryInterval is longer than the budget's: a failed analysis has
	// already spent its queries, and a report is less urgent than a budget state.
	reportRetryInterval = 5 * time.Minute
)

// UsageGetter supplies what dashboards and rules read. usage.Cache implements it.
type UsageGetter interface {
	Get(ctx context.Context) (*usage.Snapshot, error)
}

// CardinalityReportReconciler reconciles a CardinalityBudget into its
// CardinalityReport. It's separate from CardinalityBudgetReconciler so that the
// budget loop, which runs every few minutes, stays cheap.
type CardinalityReportReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Source recommend.Source
	Usage  UsageGetter
	// AdminNamespaces lists the namespaces whose budgets may target any namespace.
	// A report shows the target's metric names, so it follows the same rule.
	AdminNamespaces []string
	// Interval is how long to wait between analyses of a budget.
	Interval time.Duration
}

// +kubebuilder:rbac:groups=kardigan.paulgreco.ca,resources=cardinalitybudgets,verbs=get;list;watch
// +kubebuilder:rbac:groups=kardigan.paulgreco.ca,resources=cardinalitybudgets/finalizers,verbs=update
// +kubebuilder:rbac:groups=kardigan.paulgreco.ca,resources=cardinalityreports,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kardigan.paulgreco.ca,resources=cardinalityreports/status,verbs=patch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=prometheusrules,verbs=list

// Reconcile keeps the budget's report in step with what the target sends to Mimir
// and what dashboards and rules read. It never changes how metrics are collected.
func (r *CardinalityReportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cb kardiganv1alpha1.CardinalityBudget
	if err := r.Get(ctx, req.NamespacedName, &cb); err != nil {
		// Once the budget is gone, its owner reference gets the report deleted.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	report := &kardiganv1alpha1.CardinalityReport{
		ObjectMeta: metav1.ObjectMeta{Name: cb.Name, Namespace: cb.Namespace},
	}
	if !targetAllowed(&cb, r.AdminNamespaces) {
		// The budget reports why on its own status. Don't requeue, as it doesn't.
		return ctrl.Result{}, client.IgnoreNotFound(r.Delete(ctx, report))
	}

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, report, func() error {
		report.Spec.BudgetRef = cb.Name
		return controllerutil.SetControllerReference(&cb, report, r.Scheme)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("writing report: %w", err)
	}
	base := report.DeepCopy()

	snapshot, err := r.Usage.Get(ctx)
	if err != nil {
		log.Error(err, "collecting usage")
		setReportCondition(report, conditionReady, metav1.ConditionFalse, "UsageUnavailable",
			"Could not read every dashboard and rule; the operator's log has the detail")
		return r.patchStatus(ctx, report, base, reportRetryInterval)
	}
	if snapshot.Total() == 0 {
		// With nothing to compare against, every metric would look unused.
		setReportCondition(report, conditionReady, metav1.ConditionFalse, "NoUsageData",
			"no dashboards or rules were found, so Kardigan can't tell which metrics are used")
		return r.patchStatus(ctx, report, base, reportRetryInterval)
	}

	result, err := recommend.Analyze(ctx, r.Source, cb.Spec.Target.Label, cb.Spec.Target.Value, snapshot)
	if err != nil {
		log.Error(err, "analysing target", "label", cb.Spec.Target.Label, "value", cb.Spec.Target.Value)
		mimirQueryErrors.WithLabelValues(cb.Namespace, cb.Name).Inc()
		setReportCondition(report, conditionReady, metav1.ConditionFalse, "QueryFailed", queryFailureMessage(err))
		return r.patchStatus(ctx, report, base, reportRetryInterval)
	}

	now := metav1.Now()
	snippet, err := recommend.Snippet(cb.Name, now.Time, result)
	if err != nil {
		return ctrl.Result{}, err
	}
	report.Status.GeneratedAt = &now
	report.Status.TotalSeries = result.TotalSeries
	report.Status.PotentialSavings = result.PotentialSavings()
	report.Status.Recommendations = result.Recommendations
	report.Status.RelabelSnippet = snippet
	setReportCondition(report, conditionReady, metav1.ConditionTrue, "AnalysisSucceeded", "")
	setUsageCondition(report, snapshot)

	return r.patchStatus(ctx, report, base, r.Interval)
}

func setUsageCondition(report *kardiganv1alpha1.CardinalityReport, snapshot *usage.Snapshot) {
	switch _, dashboards := snapshot.Expressions[recommend.DashboardSource]; {
	case snapshot.ParseFailures > 0:
		setReportCondition(report, conditionUsageComplete, metav1.ConditionFalse, "ParseFailures",
			fmt.Sprintf("%d of %d expressions couldn't be parsed", snapshot.ParseFailures, snapshot.Total()))
	case !dashboards:
		setReportCondition(report, conditionUsageComplete, metav1.ConditionFalse, "DashboardsNotChecked",
			"no Grafana URL is configured, so only rules were checked")
	default:
		setReportCondition(report, conditionUsageComplete, metav1.ConditionTrue, "AllExpressionsParsed",
			fmt.Sprintf("%d expressions read", snapshot.Total()))
	}
}

// patchStatus writes status and schedules the next run. A patch can't conflict,
// and this controller is the only writer, so a report never repeats its analysis
// because of a stale read.
func (r *CardinalityReportReconciler) patchStatus(
	ctx context.Context, report, base *kardiganv1alpha1.CardinalityReport, next time.Duration,
) (ctrl.Result, error) {
	if err := r.Status().Patch(ctx, report, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("writing report status: %w", err)
	}
	return ctrl.Result{RequeueAfter: next}, nil
}

func setReportCondition(
	report *kardiganv1alpha1.CardinalityReport, condType string, status metav1.ConditionStatus, reason, message string,
) {
	meta.SetStatusCondition(&report.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: report.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager. Owned reports only
// matter when deleted, so that someone removing one gets it back straight away.
// Reacting to its other changes would start an analysis for each status write.
func (r *CardinalityReportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	onDelete := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&kardiganv1alpha1.CardinalityBudget{},
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&kardiganv1alpha1.CardinalityReport{}, builder.WithPredicates(onDelete)).
		Named("cardinalityreport").
		Complete(r)
}

// PrometheusRuleExpressions returns a function that reads the PromQL of every
// PrometheusRule in the cluster. It uses unstructured objects to avoid depending
// on the Prometheus Operator's Go module, and reader should be uncached, because
// a cached read would need the CRD to exist for the operator to start. Without
// the CRD there are simply no such rules.
func PrometheusRuleExpressions(reader client.Reader) func(ctx context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusRuleList",
		})
		if err := reader.List(ctx, list); err != nil {
			if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, err
		}

		var exprs []string
		for _, item := range list.Items {
			groups, _, err := unstructured.NestedSlice(item.Object, "spec", "groups")
			if err != nil {
				return nil, fmt.Errorf("reading PrometheusRule %s/%s: %w", item.GetNamespace(), item.GetName(), err)
			}
			for _, group := range groups {
				exprs = append(exprs, groupExpressions(group)...)
			}
		}
		return exprs, nil
	}
}

func groupExpressions(group any) []string {
	groupMap, ok := group.(map[string]any)
	if !ok {
		return nil
	}
	rules, _, _ := unstructured.NestedSlice(groupMap, "rules")
	var exprs []string
	for _, rule := range rules {
		if ruleMap, ok := rule.(map[string]any); ok {
			// expr is an int-or-string in the CRD, so a rule like "expr: 1" is valid.
			if expr, found := ruleMap["expr"]; found {
				exprs = append(exprs, fmt.Sprint(expr))
			}
		}
	}
	return exprs
}
