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
	"github.com/prometheus/client_golang/prometheus/testutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kardiganv1alpha1 "github.com/pwgreco/kardigan/api/v1alpha1"
)

// fakeCounter stands in for Mimir and records what it was asked for.
type fakeCounter struct {
	n                  int64
	err                error
	gotLabel, gotValue string
}

func (f *fakeCounter) CountSeries(_ context.Context, label, value string) (int64, error) {
	f.gotLabel, f.gotValue = label, value
	return f.n, f.err
}

var _ = Describe("CardinalityBudget Controller", func() {
	Context("When reconciling a resource", func() {
		const (
			resourceName      = "test-resource"
			resourceNamespace = "default"
			jobLabel          = "job"
			teamA             = "team-a"
		)

		ctx := context.Background()

		typeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: resourceNamespace,
		}
		cardinalitybudget := &kardiganv1alpha1.CardinalityBudget{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind CardinalityBudget")
			err := k8sClient.Get(ctx, typeNamespacedName, cardinalitybudget)
			if err != nil && apierrors.IsNotFound(err) {
				resource := &kardiganv1alpha1.CardinalityBudget{
					ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: resourceNamespace},
					Spec: kardiganv1alpha1.CardinalityBudgetSpec{
						Target:    kardiganv1alpha1.BudgetTarget{Label: jobLabel, Value: teamA},
						MaxSeries: 1000,
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &kardiganv1alpha1.CardinalityBudget{}
			err := k8sClient.Get(ctx, typeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance CardinalityBudget")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})

		reconcileWith := func(c *fakeCounter, adminNamespaces ...string) (ctrl.Result, kardiganv1alpha1.CardinalityBudget) {
			r := &CardinalityBudgetReconciler{
				Client: k8sClient, Counter: c, AdminNamespaces: adminNamespaces,
			}
			res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: typeNamespacedName})
			Expect(err).NotTo(HaveOccurred())
			var got kardiganv1alpha1.CardinalityBudget
			Expect(k8sClient.Get(ctx, typeNamespacedName, &got)).To(Succeed())
			return res, got
		}

		condition := func(cb kardiganv1alpha1.CardinalityBudget, condType string) metav1.Condition {
			cond := meta.FindStatusCondition(cb.Status.Conditions, condType)
			Expect(cond).NotTo(BeNil(), "condition %s is missing", condType)
			return *cond
		}

		It("reports a budget within its limit", func() {
			counter := &fakeCounter{n: 100}
			res, got := reconcileWith(counter)
			Expect(counter.gotLabel).To(Equal(jobLabel))
			Expect(counter.gotValue).To(Equal(teamA))
			Expect(got.Status.CurrentSeries).To(Equal(int64(100)))
			Expect(got.Status.UsagePercent).To(Equal(int32(10)))
			Expect(got.Status.LastChecked).NotTo(BeNil())
			Expect(got.Status.ObservedGeneration).To(Equal(got.Generation))
			cond := condition(got, conditionWithinBudget)
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("WithinBudget"))
			Expect(condition(got, conditionReady).Status).To(Equal(metav1.ConditionTrue))
			Expect(res.RequeueAfter).To(Equal(5 * time.Minute))
		})

		It("warns near the limit", func() {
			_, got := reconcileWith(&fakeCounter{n: 850})
			cond := condition(got, conditionWithinBudget)
			Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			Expect(cond.Reason).To(Equal("WarningThreshold"))
		})

		It("marks a budget over its limit", func() {
			_, got := reconcileWith(&fakeCounter{n: 1500})
			Expect(got.Status.UsagePercent).To(Equal(int32(150)))
			cond := condition(got, conditionWithinBudget)
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("BudgetExceeded"))
		})

		It("records a Mimir failure without returning an error", func() {
			before := testutil.ToFloat64(mimirQueryErrors.WithLabelValues(resourceNamespace, resourceName))
			res, got := reconcileWith(&fakeCounter{err: errors.New("connection refused")})
			cond := condition(got, conditionReady)
			Expect(cond.Status).To(Equal(metav1.ConditionFalse))
			Expect(cond.Reason).To(Equal("QueryFailed"))
			// Upstream detail stays in the log: a budget's readers may not be allowed it.
			Expect(cond.Message).NotTo(ContainSubstring("connection refused"))
			Expect(cond.Message).To(ContainSubstring("operator's log"))
			Expect(got.Status.LastChecked).To(BeNil())
			Expect(res.RequeueAfter).To(Equal(time.Minute))
			Expect(testutil.ToFloat64(mimirQueryErrors.WithLabelValues(resourceNamespace, resourceName))).
				To(Equal(before + 1))
		})

		It("recovers once Mimir answers again", func() {
			reconcileWith(&fakeCounter{err: errors.New("connection refused")})
			_, got := reconcileWith(&fakeCounter{n: 100})
			Expect(condition(got, conditionReady).Status).To(Equal(metav1.ConditionTrue))
		})

		It("requeues after the budget's own interval", func() {
			var cb kardiganv1alpha1.CardinalityBudget
			Expect(k8sClient.Get(ctx, typeNamespacedName, &cb)).To(Succeed())
			cb.Spec.Interval = metav1.Duration{Duration: 30 * time.Second}
			Expect(k8sClient.Update(ctx, &cb)).To(Succeed())

			res, _ := reconcileWith(&fakeCounter{n: 100})
			Expect(res.RequeueAfter).To(Equal(30 * time.Second))
		})

		DescribeTable("validates the interval",
			func(interval time.Duration, wantValid bool) {
				var cb kardiganv1alpha1.CardinalityBudget
				Expect(k8sClient.Get(ctx, typeNamespacedName, &cb)).To(Succeed())
				cb.Spec.Interval = metav1.Duration{Duration: interval}
				err := k8sClient.Update(ctx, &cb)
				if wantValid {
					Expect(err).NotTo(HaveOccurred())
					return
				}
				Expect(apierrors.IsInvalid(err)).To(BeTrue(), "err = %v", err)
				Expect(err.Error()).To(ContainSubstring("interval must be at least 30s"))
			},
			Entry("below the floor", 29*time.Second, false),
			Entry("at the floor", 30*time.Second, true),
			Entry("above the floor", time.Hour, true),
		)

		It("defaults an unset interval", func() {
			var cb kardiganv1alpha1.CardinalityBudget
			Expect(k8sClient.Get(ctx, typeNamespacedName, &cb)).To(Succeed())
			Expect(cb.Spec.Interval.Duration).To(Equal(5 * time.Minute))
		})

		It("exports the budget's metrics", func() {
			reconcileWith(&fakeCounter{n: 1500})
			labels := []string{resourceNamespace, resourceName, jobLabel, teamA}
			Expect(testutil.ToFloat64(budgetSeries.WithLabelValues(labels...))).To(Equal(1500.0))
			Expect(testutil.ToFloat64(budgetMaxSeries.WithLabelValues(labels...))).To(Equal(1000.0))
			Expect(testutil.ToFloat64(budgetUsageRatio.WithLabelValues(labels...))).To(Equal(1.5))
		})

		setTarget := func(label, value string) {
			var cb kardiganv1alpha1.CardinalityBudget
			Expect(k8sClient.Get(ctx, typeNamespacedName, &cb)).To(Succeed())
			cb.Spec.Target = kardiganv1alpha1.BudgetTarget{Label: label, Value: value}
			Expect(k8sClient.Update(ctx, &cb)).To(Succeed())
		}

		It("replaces the old series when the target changes", func() {
			reconcileWith(&fakeCounter{n: 100})
			before := testutil.CollectAndCount(budgetSeries)

			setTarget(jobLabel, "team-z")
			reconcileWith(&fakeCounter{n: 200})
			Expect(testutil.CollectAndCount(budgetSeries)).To(Equal(before))
			labels := []string{resourceNamespace, resourceName, jobLabel, "team-z"}
			Expect(testutil.ToFloat64(budgetSeries.WithLabelValues(labels...))).To(Equal(200.0))
		})

		It("clears a budget's gauges only when its spec changed", func() {
			reconcileWith(&fakeCounter{n: 100})
			marker := []string{resourceNamespace, resourceName, jobLabel, "marker"}
			budgetSeries.WithLabelValues(marker...).Set(1)
			before := testutil.CollectAndCount(budgetSeries)

			reconcileWith(&fakeCounter{n: 100})
			Expect(testutil.CollectAndCount(budgetSeries)).To(Equal(before), "an unchanged spec must not clear gauges")

			setTarget(jobLabel, "team-z")
			reconcileWith(&fakeCounter{n: 100})
			Expect(testutil.CollectAndCount(budgetSeries)).To(Equal(before - 1))
		})

		It("leaves other budgets' gauges alone when clearing one", func() {
			other := []string{"other-namespace", resourceName, jobLabel, teamA}
			budgetSeries.WithLabelValues(other...).Set(42)
			DeferCleanup(func() { budgetSeries.DeleteLabelValues(other...) })

			deleteBudgetGauges(resourceNamespace, resourceName)
			Expect(testutil.ToFloat64(budgetSeries.WithLabelValues(other...))).To(Equal(42.0))
		})

		DescribeTable("decides which targets a budget may count",
			func(label, value string, adminNamespaces []string, wantAllowed bool) {
				setTarget(label, value)
				counter := &fakeCounter{n: 100}
				res, got := reconcileWith(counter, adminNamespaces...)
				if wantAllowed {
					Expect(counter.gotValue).To(Equal(value))
					Expect(condition(got, conditionReady).Status).To(Equal(metav1.ConditionTrue))
					return
				}
				Expect(counter.gotValue).To(BeEmpty(), "Mimir should not be queried")
				cond := condition(got, conditionReady)
				Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				Expect(cond.Reason).To(Equal("CrossNamespaceTarget"))
				Expect(res.RequeueAfter).To(BeZero())
			},
			Entry("own namespace", "namespace", resourceNamespace, nil, true),
			Entry("another namespace", "namespace", "team-b", nil, false),
			Entry("another namespace, other namespaces are admins", "namespace", "team-b", []string{"sre"}, false),
			Entry("another namespace, from an admin namespace", "namespace", "team-b", []string{"sre", resourceNamespace}, true),
			Entry("a job has no owning namespace", jobLabel, "team-b", nil, true),
		)

		It("clears what a budget reported before it became cross-namespace", func() {
			reconcileWith(&fakeCounter{n: 100})
			before := testutil.CollectAndCount(budgetSeries)

			setTarget("namespace", "team-b")
			_, got := reconcileWith(&fakeCounter{n: 100})
			Expect(got.Status.CurrentSeries).To(BeZero())
			Expect(got.Status.LastChecked).To(BeNil())
			Expect(meta.FindStatusCondition(got.Status.Conditions, conditionWithinBudget)).To(BeNil())
			Expect(testutil.CollectAndCount(budgetSeries)).To(Equal(before - 1))
		})

		It("removes a deleted budget's metrics", func() {
			name := types.NamespacedName{Name: "deleted-budget", Namespace: resourceNamespace}
			resource := &kardiganv1alpha1.CardinalityBudget{
				ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace},
				Spec: kardiganv1alpha1.CardinalityBudgetSpec{
					Target:    kardiganv1alpha1.BudgetTarget{Label: jobLabel, Value: "team-b"},
					MaxSeries: 1000,
				},
			}
			Expect(k8sClient.Create(ctx, resource)).To(Succeed())

			r := &CardinalityBudgetReconciler{Client: k8sClient, Counter: &fakeCounter{n: 1}}
			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			before := testutil.CollectAndCount(budgetSeries)

			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
			_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: name})
			Expect(err).NotTo(HaveOccurred())
			Expect(testutil.CollectAndCount(budgetSeries)).To(Equal(before - 1))
		})
	})
})
