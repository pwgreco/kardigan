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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	kardiganv1alpha1 "github.com/pwgreco/kardigan/api/v1alpha1"
)

const (
	metricLabelNamespace = "namespace"
	metricLabelName      = "name"
)

// budgetLabels carries the target as well as the budget's identity: a budget
// managed centrally lives outside the namespace it measures.
var budgetLabels = []string{metricLabelNamespace, metricLabelName, "target_label", "target_value"}

var (
	budgetSeries = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kardigan_budget_series",
		Help: "Active series counted for the budget's target.",
	}, budgetLabels)

	budgetMaxSeries = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kardigan_budget_max_series",
		Help: "The budget's maxSeries.",
	}, budgetLabels)

	budgetUsageRatio = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kardigan_budget_usage_ratio",
		Help: "Active series divided by maxSeries. Above 1 means over budget.",
	}, budgetLabels)

	mimirQueryErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kardigan_mimir_query_errors_total",
		Help: "Failed Mimir queries, by budget.",
	}, []string{metricLabelNamespace, metricLabelName})
)

func init() {
	// controller-runtime serves this registry on the metrics endpoint.
	metrics.Registry.MustRegister(budgetSeries, budgetMaxSeries, budgetUsageRatio, mimirQueryErrors)
}

// setBudgetGauges records a budget's latest count. Call it before updating
// status.observedGeneration: a spec edit may have changed the target, which would
// leave the old series reporting a stale value forever, so it clears them first.
// It clears only then because a scrape landing between the delete and the set
// would see a gap, which resets an alert's "for" timer.
func setBudgetGauges(cb *kardiganv1alpha1.CardinalityBudget, count int64) {
	if cb.Status.ObservedGeneration != cb.Generation {
		deleteBudgetGauges(cb.Namespace, cb.Name)
	}
	labels := []string{cb.Namespace, cb.Name, cb.Spec.Target.Label, cb.Spec.Target.Value}
	budgetSeries.WithLabelValues(labels...).Set(float64(count))
	budgetMaxSeries.WithLabelValues(labels...).Set(float64(cb.Spec.MaxSeries))
	budgetUsageRatio.WithLabelValues(labels...).Set(float64(count) / float64(cb.Spec.MaxSeries))
}

// deleteBudgetGauges removes a budget's gauges. It matches on identity alone
// because the target may no longer be known.
func deleteBudgetGauges(namespace, name string) {
	identity := prometheus.Labels{metricLabelNamespace: namespace, metricLabelName: name}
	budgetSeries.DeletePartialMatch(identity)
	budgetMaxSeries.DeletePartialMatch(identity)
	budgetUsageRatio.DeletePartialMatch(identity)
}

// deleteBudgetMetrics removes a deleted budget's series so it doesn't report
// a stale value forever.
func deleteBudgetMetrics(namespace, name string) {
	deleteBudgetGauges(namespace, name)
	mimirQueryErrors.DeleteLabelValues(namespace, name)
}
