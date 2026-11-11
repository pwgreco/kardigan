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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kardiganv1alpha1 "github.com/pwgreco/kardigan/api/v1alpha1"
	"github.com/pwgreco/kardigan/internal/mimir"
	"github.com/pwgreco/kardigan/internal/recommend"
	"github.com/pwgreco/kardigan/internal/usage"
)

// fakeAnalysisSource stands in for Mimir's analysis endpoints.
type fakeAnalysisSource struct {
	metrics []mimir.NameCount
	total   int64
	err     error
	calls   int
}

func (f *fakeAnalysisSource) SeriesPerMetric(context.Context, string, string) ([]mimir.NameCount, int64, error) {
	f.calls++
	return f.metrics, f.total, f.err
}

func (f *fakeAnalysisSource) LabelValueCounts(context.Context, string) ([]mimir.NameCount, error) {
	return nil, nil
}

func (f *fakeAnalysisSource) QueryCount(context.Context, string) (int64, error) {
	return 0, nil
}

// fakeUsage returns a fixed snapshot, or an error.
type fakeUsage struct {
	snapshot *usage.Snapshot
	err      error
}

func (f *fakeUsage) Get(context.Context) (*usage.Snapshot, error) {
	return f.snapshot, f.err
}

var _ = Describe("CardinalityReport Controller", func() {
	const (
		name        = "report-test"
		namespace   = "default"
		usedMetric  = "used_total"
		unusedName  = "unused_total"
		dropSavings = 900
		reportEvery = 2 * time.Hour
	)

	ctx := context.Background()
	key := types.NamespacedName{Name: name, Namespace: namespace}

	dashboardsReading := func(exprs ...string) *fakeUsage {
		snapshot, err := usage.Collect(ctx, []usage.Source{{
			Name: recommend.DashboardSource,
			Fetch: func(context.Context) ([]string, error) {
				return exprs, nil
			},
		}})
		Expect(err).NotTo(HaveOccurred())
		return &fakeUsage{snapshot: snapshot}
	}

	source := func() *fakeAnalysisSource {
		return &fakeAnalysisSource{
			metrics: []mimir.NameCount{{Name: unusedName, Count: dropSavings}, {Name: usedMetric, Count: 100}},
			total:   dropSavings + 100,
		}
	}

	reconcileWith := func(
		src recommend.Source, used UsageGetter, adminNamespaces ...string,
	) ctrl.Result {
		r := &CardinalityReportReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Source: src, Usage: used,
			AdminNamespaces: adminNamespaces, Interval: reportEvery,
		}
		res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		return res
	}

	getReport := func() *kardiganv1alpha1.CardinalityReport {
		report := &kardiganv1alpha1.CardinalityReport{}
		Expect(k8sClient.Get(ctx, key, report)).To(Succeed())
		return report
	}

	reportCondition := func(report *kardiganv1alpha1.CardinalityReport, condType string) metav1.Condition {
		cond := meta.FindStatusCondition(report.Status.Conditions, condType)
		Expect(cond).NotTo(BeNil(), "condition %s is missing", condType)
		return *cond
	}

	BeforeEach(func() {
		Expect(k8sClient.Create(ctx, &kardiganv1alpha1.CardinalityBudget{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kardiganv1alpha1.CardinalityBudgetSpec{
				Target:    kardiganv1alpha1.BudgetTarget{Label: "job", Value: "team-a"},
				MaxSeries: 1000,
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		budget := &kardiganv1alpha1.CardinalityBudget{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		Expect(k8sClient.Delete(ctx, budget)).To(Succeed())
		// envtest runs no garbage collector, so remove the report by hand.
		report := &kardiganv1alpha1.CardinalityReport{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		if err := k8sClient.Delete(ctx, report); err != nil {
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}
	})

	It("writes a report owned by the budget, and reruns after the interval", func() {
		res := reconcileWith(source(), dashboardsReading(usedMetric))
		Expect(res.RequeueAfter).To(Equal(reportEvery))

		report := getReport()
		Expect(report.Spec.BudgetRef).To(Equal(name))

		var budget kardiganv1alpha1.CardinalityBudget
		Expect(k8sClient.Get(ctx, key, &budget)).To(Succeed())
		Expect(controllerutil.HasControllerReference(report)).To(BeTrue())
		Expect(report.OwnerReferences[0].UID).To(Equal(budget.UID))

		Expect(report.Status.GeneratedAt).NotTo(BeNil())
		Expect(report.Status.TotalSeries).To(Equal(int64(dropSavings + 100)))
		Expect(report.Status.PotentialSavings).To(Equal(int64(dropSavings)))
		Expect(report.Status.Recommendations).To(HaveLen(2))
		Expect(report.Status.Recommendations[0].Action).To(Equal(kardiganv1alpha1.ActionDropMetric))
		Expect(report.Status.Recommendations[0].Metric).To(Equal(unusedName))
		Expect(report.Status.Recommendations[1].Action).To(Equal(kardiganv1alpha1.ActionKeep))
		Expect(report.Status.Recommendations[1].Metric).To(Equal(usedMetric))
		Expect(report.Status.RelabelSnippet).To(ContainSubstring("regex: unused_total"))

		Expect(reportCondition(report, conditionReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(reportCondition(report, conditionUsageComplete).Status).To(Equal(metav1.ConditionTrue))
	})

	It("doesn't recommend anything when no usage data exists, as every metric would look unused", func() {
		src := source()
		res := reconcileWith(src, dashboardsReading())

		Expect(res.RequeueAfter).To(Equal(reportRetryInterval))
		Expect(src.calls).To(BeZero())
		report := getReport()
		Expect(report.Status.Recommendations).To(BeEmpty())
		Expect(reportCondition(report, conditionReady).Reason).To(Equal("NoUsageData"))
	})

	It("doesn't recommend anything when usage can't be read", func() {
		src := source()
		res := reconcileWith(src, &fakeUsage{err: errors.New("grafana is down")})

		Expect(res.RequeueAfter).To(Equal(reportRetryInterval))
		Expect(src.calls).To(BeZero())
		report := getReport()
		Expect(report.Status.Recommendations).To(BeEmpty())
		cond := reportCondition(report, conditionReady)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("UsageUnavailable"))
		// Upstream detail stays in the log: a report's readers may not be allowed it.
		Expect(cond.Message).NotTo(ContainSubstring("grafana is down"))
	})

	It("keeps the last good recommendations when a later run fails, and recovers", func() {
		reconcileWith(source(), dashboardsReading(usedMetric))
		good := getReport()

		failing := source()
		failing.err = errors.New("mimir is down")
		res := reconcileWith(failing, dashboardsReading(usedMetric))
		Expect(res.RequeueAfter).To(Equal(reportRetryInterval))

		report := getReport()
		cond := reportCondition(report, conditionReady)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("QueryFailed"))
		Expect(report.Status.Recommendations).To(Equal(good.Status.Recommendations))
		Expect(report.Status.GeneratedAt.Time).To(BeTemporally("==", good.Status.GeneratedAt.Time))

		reconcileWith(source(), dashboardsReading(usedMetric))
		Expect(reportCondition(getReport(), conditionReady).Status).To(Equal(metav1.ConditionTrue))
	})

	It("says when usage is incomplete", func() {
		reconcileWith(source(), dashboardsReading(usedMetric, "rate("))
		cond := reportCondition(getReport(), conditionUsageComplete)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("ParseFailures"))
		Expect(cond.Message).To(ContainSubstring("1 of 2"))
	})

	It("says when dashboards weren't checked", func() {
		snapshot, err := usage.Collect(ctx, []usage.Source{{
			Name:  "mimir-ruler",
			Fetch: func(context.Context) ([]string, error) { return []string{usedMetric}, nil },
		}})
		Expect(err).NotTo(HaveOccurred())

		reconcileWith(source(), &fakeUsage{snapshot: snapshot})
		Expect(reportCondition(getReport(), conditionUsageComplete).Reason).To(Equal("DashboardsNotChecked"))
	})

	Context("with a budget targeting another namespace", func() {
		retarget := func() {
			var budget kardiganv1alpha1.CardinalityBudget
			Expect(k8sClient.Get(ctx, key, &budget)).To(Succeed())
			budget.Spec.Target = kardiganv1alpha1.BudgetTarget{Label: targetLabelNamespace, Value: "other"}
			Expect(k8sClient.Update(ctx, &budget)).To(Succeed())
		}

		It("doesn't show that namespace's metric names", func() {
			retarget()
			src := source()
			res := reconcileWith(src, dashboardsReading(usedMetric))

			Expect(res.RequeueAfter).To(BeZero())
			Expect(src.calls).To(BeZero())
			err := k8sClient.Get(ctx, key, &kardiganv1alpha1.CardinalityReport{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("removes the report it had before", func() {
			reconcileWith(source(), dashboardsReading(usedMetric))
			getReport()

			retarget()
			reconcileWith(source(), dashboardsReading(usedMetric))
			err := k8sClient.Get(ctx, key, &kardiganv1alpha1.CardinalityReport{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("allows it from an admin namespace", func() {
			retarget()
			reconcileWith(source(), dashboardsReading(usedMetric), namespace)
			Expect(getReport().Status.Recommendations).NotTo(BeEmpty())
		})
	})

	It("does nothing once the budget is gone", func() {
		r := &CardinalityReportReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Source: source(),
			Usage: dashboardsReading(usedMetric), Interval: reportEvery,
		}
		res, err := r.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "no-such-budget", Namespace: namespace},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(res).To(Equal(ctrl.Result{}))
	})

	// envtest has no PrometheusRule CRD, which is the same as a cluster without the
	// Prometheus Operator.
	It("treats a cluster without the PrometheusRule CRD as having no such rules", func() {
		exprs, err := PrometheusRuleExpressions(k8sClient)(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(exprs).To(BeEmpty())
	})

	It("reads expressions out of a PrometheusRule's groups", func() {
		group := map[string]any{"name": "g", "rules": []any{
			map[string]any{"record": "r", "expr": "sum(a_total)"},
			map[string]any{"alert": "A", "expr": int64(1)},
			map[string]any{"record": "no expression"},
		}}
		Expect(groupExpressions(group)).To(Equal([]string{"sum(a_total)", "1"}))
		Expect(groupExpressions("not a map")).To(BeEmpty())
	})
})
