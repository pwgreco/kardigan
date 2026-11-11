//go:build stack

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

// Package stack tests the deployed operator against real Mimir and Grafana. It
// needs the cluster and local stack that hack/test-stack.sh sets up, so run it with
// make test-stack or make test-extended. The tests run in order, because the outages
// restart things and some tests add series to Mimir.
package stack

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	kardiganv1alpha1 "github.com/pwgreco/kardigan/api/v1alpha1"
)

const (
	operatorDeploy = "deploy/kardigan-controller-manager"
	metricsService = "svc/kardigan-controller-manager-metrics-service"
	metricsPort    = "18443"
	teamA          = "team-a"
	teamB          = "team-b"
	teamC          = "team-c"

	// usedMetric is what the demo dashboard reads.
	usedMetric = "avalanche_histogram_metric_mmmmm_0_0_bucket"

	prometheusRuleCRD = "https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/" +
		"v0.94.2/example/prometheus-operator-crd/monitoring.coreos.com_prometheusrules.yaml"
)

// TestStack runs each behaviour in order, and prints the operator's log on failure.
func TestStack(t *testing.T) {
	logOperatorOnFailure(t, operatorDeploy)
	if out, err := kubectl(t, "apply", "-k", "../../config/samples/"); err != nil {
		t.Fatalf("applying samples: %v\n%s", err, out)
	}

	t.Run("budgets report the expected states", testBudgetStates)
	t.Run("reports give the expected advice", testReports)
	t.Run("a spec edit applies straight away", testSpecEdit)
	t.Run("the API server rejects bad budgets", testValidation)
	t.Run("cross-namespace targets need an admin namespace", testCrossNamespace)
	t.Run("deleting a budget removes its metrics and report", testDeletion)
	t.Run("budgets by namespace use real series", testNamespaceTargets)
	t.Run("a PrometheusRule marks a metric as used", testPrometheusRule)
	t.Run("a Grafana outage is reported and recovers", testGrafanaOutage)
	t.Run("a Mimir outage is reported and recovers", testOutage)
}

func testBudgetStates(t *testing.T) {
	tests := []struct {
		name   string
		series int64
		status metav1.ConditionStatus
		reason string
	}{
		{teamA, 500, metav1.ConditionTrue, condWithinBudget},
		{teamB, 2000, metav1.ConditionTrue, "WarningThreshold"},
		{teamC, 15750, metav1.ConditionFalse, "BudgetExceeded"},
		{"team-d", 1000, metav1.ConditionTrue, condWithinBudget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cb kardiganv1alpha1.CardinalityBudget
			eventually(t, settleTimeout, func() error {
				if err := getBudget(t, testNamespace, tt.name, &cb); err != nil {
					return err
				}
				if err := wantCondition(cb.Status.Conditions, condReady, metav1.ConditionTrue, "QuerySucceeded"); err != nil {
					return err
				}
				return wantCondition(cb.Status.Conditions, condWithinBudget, tt.status, tt.reason)
			})
			// Avalanche's flags fix the counts, and Prometheus adds a few per job.
			// Checking them catches a count that isn't scoped to the team.
			if got := cb.Status.CurrentSeries; got < tt.series || got > tt.series+tt.series/20 {
				t.Errorf("currentSeries = %d, want about %d", got, tt.series)
			}
		})
	}
}

func testReports(t *testing.T) {
	tests := []struct {
		name string
		want []string
	}{
		{teamA, []string{kardiganv1alpha1.ActionDropMetric}},
		{teamB, []string{kardiganv1alpha1.ActionDropMetric}},
		{teamC, []string{kardiganv1alpha1.ActionDropMetric, kardiganv1alpha1.ActionFixLabel, kardiganv1alpha1.ActionKeep}},
		{"team-d", []string{kardiganv1alpha1.ActionDropLabel, kardiganv1alpha1.ActionTrimHistogram}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := readyReport(t, testNamespace, tt.name)

			actions := map[string]bool{}
			for _, rec := range report.Status.Recommendations {
				actions[rec.Action] = true
				if rec.Action == kardiganv1alpha1.ActionDropMetric && rec.Metric == usedMetric {
					t.Errorf("recommends dropping %s, which a dashboard reads", usedMetric)
				}
			}
			for _, want := range tt.want {
				if !actions[want] {
					t.Errorf("no %s recommendation in %v", want, report.Status.Recommendations)
				}
			}
			if err := wantCondition(report.Status.Conditions, condUsage, metav1.ConditionTrue, "AllExpressionsParsed"); err != nil {
				t.Error(err)
			}
			if report.Status.PotentialSavings <= 0 {
				t.Errorf("potentialSavings = %d, want more than 0", report.Status.PotentialSavings)
			}
			if !strings.Contains(report.Status.RelabelSnippet, "metricRelabelings:") {
				t.Errorf("relabelSnippet = %q, want metricRelabelings", report.Status.RelabelSnippet)
			}
		})
	}

	// Used through the dashboard, so it stays, and says so. Only team-c lists it,
	// because Keep entries are capped and sorted by size.
	report := readyReport(t, testNamespace, teamC)
	if !slices.ContainsFunc(report.Status.Recommendations, func(r kardiganv1alpha1.Recommendation) bool {
		return r.Action == kardiganv1alpha1.ActionKeep && r.Metric == usedMetric
	}) {
		t.Errorf("team-c has no Keep for %s: %v", usedMetric, report.Status.Recommendations)
	}
}

func testSpecEdit(t *testing.T) {
	setMaxSeries(t, testNamespace, teamC, 20000)
	t.Cleanup(func() { setMaxSeries(t, testNamespace, teamC, 10000) })

	// Well under the 5m interval, so this can only come from the spec change.
	eventually(t, 30*time.Second, func() error {
		var cb kardiganv1alpha1.CardinalityBudget
		if err := getBudget(t, testNamespace, teamC, &cb); err != nil {
			return err
		}
		return wantCondition(cb.Status.Conditions, condWithinBudget, metav1.ConditionTrue, condWithinBudget)
	})
}

func testValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*kardiganv1alpha1.CardinalityBudget)
	}{
		{"unknown label", func(cb *kardiganv1alpha1.CardinalityBudget) { cb.Spec.Target.Label = "pod" }},
		{"empty value", func(cb *kardiganv1alpha1.CardinalityBudget) { cb.Spec.Target.Value = "" }},
		{"value too long", func(cb *kardiganv1alpha1.CardinalityBudget) { cb.Spec.Target.Value = strings.Repeat("x", 254) }},
		{"zero maxSeries", func(cb *kardiganv1alpha1.CardinalityBudget) { cb.Spec.MaxSeries = 0 }},
		{"interval below the floor", func(cb *kardiganv1alpha1.CardinalityBudget) {
			cb.Spec.Interval = metav1.Duration{Duration: 10 * time.Second}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cb := newBudget(testNamespace, "invalid", "job", "x")
			tt.mutate(cb)
			err := k8s.Create(context.Background(), cb)
			if !apierrors.IsInvalid(err) {
				t.Errorf("err = %v, want the API server to reject it", err)
			}
		})
	}
}

func testCrossNamespace(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"sre", "team-x"} {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		if err := k8s.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}

	denied := newBudget(testNamespace, "other-team", "namespace", "team-x")
	allowed := newBudget("sre", "team-x", "namespace", "team-x")
	for _, cb := range []*kardiganv1alpha1.CardinalityBudget{denied, allowed} {
		if err := k8s.Create(ctx, cb); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8s.Delete(ctx, cb) })
	}

	eventually(t, settleTimeout, func() error {
		var cb kardiganv1alpha1.CardinalityBudget
		if err := getBudget(t, testNamespace, "other-team", &cb); err != nil {
			return err
		}
		return wantCondition(cb.Status.Conditions, condReady, metav1.ConditionFalse, "CrossNamespaceTarget")
	})
	eventually(t, settleTimeout, func() error {
		var cb kardiganv1alpha1.CardinalityBudget
		if err := getBudget(t, "sre", "team-x", &cb); err != nil {
			return err
		}
		return wantCondition(cb.Status.Conditions, condReady, metav1.ConditionTrue, "QuerySucceeded")
	})

	// A report shows the target's metric names, so a denied budget gets none.
	var report kardiganv1alpha1.CardinalityReport
	key := types.NamespacedName{Namespace: testNamespace, Name: "other-team"}
	if err := k8s.Get(ctx, key, &report); !apierrors.IsNotFound(err) {
		t.Errorf("getting the denied budget's report: err = %v, want NotFound", err)
	}
}

func testDeletion(t *testing.T) {
	ctx := context.Background()
	readyReport(t, testNamespace, teamA)

	metrics := metricsReader(t)
	const series = `kardigan_budget_usage_ratio{name="team-a",namespace="default",target_label="job",target_value="avalanche-team-a"}`
	if body := metrics(); !strings.Contains(body, series) {
		t.Fatalf("metrics have no %s:\n%s", series, grepKardigan(body))
	}

	if err := k8s.Delete(ctx, newBudget(testNamespace, teamA, "job", "avalanche-team-a")); err != nil {
		t.Fatal(err)
	}
	eventually(t, settleTimeout, func() error {
		if body := metrics(); strings.Contains(body, `name="team-a"`) {
			return fmt.Errorf("team-a's series are still there:\n%s", grepKardigan(body))
		}
		return nil
	})
	eventually(t, settleTimeout, func() error {
		var report kardiganv1alpha1.CardinalityReport
		err := k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: teamA}, &report)
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("report still exists, err = %v", err)
	})
}

func testNamespaceTargets(t *testing.T) {
	ctx := context.Background()
	mustKubectl(t, "apply", "-f", "../../hack/kind/workloads.yaml")
	mustKubectl(t, "apply", "-f", "../../hack/kind/prometheus-agent.yaml")

	teams := []struct {
		name   string
		series int64
		status metav1.ConditionStatus
		reason string
	}{
		{teamA, 500, metav1.ConditionTrue, condWithinBudget},
		{"team-b", 2000, metav1.ConditionTrue, "WarningThreshold"},
		{teamC, 15750, metav1.ConditionFalse, "BudgetExceeded"},
	}
	for _, team := range teams {
		waitForSeries(t, team.name, team.series)
	}
	mustKubectl(t, "apply", "-f", "../../config/samples/namespace-budgets.yaml")

	for _, team := range teams {
		t.Run(team.name, func(t *testing.T) {
			var cb kardiganv1alpha1.CardinalityBudget
			eventually(t, settleTimeout, func() error {
				if err := getBudget(t, team.name, team.name, &cb); err != nil {
					return err
				}
				return wantCondition(cb.Status.Conditions, condWithinBudget, team.status, team.reason)
			})
			if got := cb.Status.CurrentSeries; got < team.series || got > team.series+team.series/20 {
				t.Errorf("currentSeries = %d, want about %d", got, team.series)
			}

			report := readyReport(t, team.name, team.name)
			if report.Status.TotalSeries < team.series {
				t.Errorf("report's totalSeries = %d, want at least %d", report.Status.TotalSeries, team.series)
			}
			if !hasAction(report, kardiganv1alpha1.ActionDropMetric) {
				t.Errorf("no DropMetric in %v", report.Status.Recommendations)
			}
		})
	}

	// With real data to find, another team's namespace is still off limits.
	denied := newBudget(teamA, "nosy", namespaceLabel, "team-b")
	if err := k8s.Create(ctx, denied); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, denied) })
	eventually(t, settleTimeout, func() error {
		var cb kardiganv1alpha1.CardinalityBudget
		if err := getBudget(t, teamA, "nosy", &cb); err != nil {
			return err
		}
		if err := wantCondition(cb.Status.Conditions, condReady, metav1.ConditionFalse, "CrossNamespaceTarget"); err != nil {
			return err
		}
		if cb.Status.CurrentSeries != 0 {
			return fmt.Errorf("currentSeries = %d, which reveals another team's data", cb.Status.CurrentSeries)
		}
		return nil
	})
}

func testPrometheusRule(t *testing.T) {
	ctx := context.Background()

	// The CRD arrives after the operator started, which it should pick up.
	mustKubectl(t, "apply", "--server-side", "-f", prometheusRuleCRD)
	mustKubectl(t, "wait", "--for=condition=established", "--timeout=60s", "crd/prometheusrules.monitoring.coreos.com")

	before := readyReport(t, testNamespace, teamB)
	var metric string
	for _, rec := range before.Status.Recommendations {
		if rec.Action == kardiganv1alpha1.ActionDropMetric {
			metric = rec.Metric
			break
		}
	}
	if metric == "" {
		t.Fatalf("team-b has no DropMetric to test with: %v", before.Status.Recommendations)
	}

	rule := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "monitoring.coreos.com/v1",
		"kind":       "PrometheusRule",
		"metadata":   map[string]any{"name": "stack-test", "namespace": testNamespace},
		"spec": map[string]any{"groups": []any{map[string]any{
			"name":  "stack-test",
			"rules": []any{map[string]any{"alert": "StackTest", "expr": metric + " > 0"}},
		}}},
	}}
	eventually(t, settleTimeout, func() error { return k8s.Create(ctx, rule) })
	t.Cleanup(func() { _ = k8s.Delete(ctx, rule) })

	// Usage is cached for the report interval, so restart to read the rule now.
	since := restartOperator(t)
	eventually(t, reportTimeout, func() error {
		report, err := freshReport(ctx, teamB, since)
		if err != nil {
			return err
		}
		for _, rec := range report.Status.Recommendations {
			if rec.Action == kardiganv1alpha1.ActionDropMetric && rec.Metric == metric {
				return fmt.Errorf("still recommends dropping %s, which a PrometheusRule reads", metric)
			}
		}
		return nil
	})
}

func testGrafanaOutage(t *testing.T) {
	ctx := context.Background()
	before := readyReport(t, testNamespace, teamB)

	docker(t, "stop", grafanaContainer)
	started := false
	start := func() {
		if !started {
			docker(t, "start", grafanaContainer)
			started = true
		}
	}
	t.Cleanup(start)

	// Dashboards are cached, so restart to make the operator read them again.
	stopped := time.Now()
	restartOperator(t)

	// A dashboard it can't read might use any metric, so the report must fail rather
	// than call them unused. It keeps its last good recommendations.
	eventually(t, reportTimeout, func() error {
		var got kardiganv1alpha1.CardinalityReport
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: teamB}, &got); err != nil {
			return err
		}
		if err := wantCondition(got.Status.Conditions, condReady, metav1.ConditionFalse, "UsageUnavailable"); err != nil {
			return err
		}
		if len(got.Status.Recommendations) != len(before.Status.Recommendations) {
			t.Errorf("%d recommendations during the outage, want the last good %d",
				len(got.Status.Recommendations), len(before.Status.Recommendations))
		}
		wantNoUpstreamDetail(t, meta.FindStatusCondition(got.Status.Conditions, condReady).Message)
		return nil
	})

	t.Logf("the report failed %s after Grafana stopped", time.Since(stopped).Round(time.Second))

	// Budgets don't need Grafana, so they carry on.
	var cb kardiganv1alpha1.CardinalityBudget
	if err := getBudget(t, testNamespace, teamB, &cb); err != nil {
		t.Fatal(err)
	}
	if err := wantCondition(cb.Status.Conditions, condReady, metav1.ConditionTrue, "QuerySucceeded"); err != nil {
		t.Error(err)
	}

	start()
	until := func() error {
		var health *http.Response
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:9000/api/health", nil)
		if err != nil {
			return err
		}
		if health, err = http.DefaultClient.Do(req); err != nil {
			return err
		}
		return health.Body.Close()
	}
	eventually(t, settleTimeout, until)

	// A spec edit makes the report analyse now, not after its 5 minute retry. The
	// first few can still fail, because the cluster's DNS can cache that Grafana's
	// name didn't resolve, so keep editing until one gets through.
	recovering := time.Now()
	t.Cleanup(func() { setMaxSeries(t, testNamespace, teamB, 2200) })
	maxSeries := int64(2300)
	eventually(t, settleTimeout, func() error {
		maxSeries++
		setMaxSeries(t, testNamespace, teamB, maxSeries)
		time.Sleep(5 * time.Second)
		var got kardiganv1alpha1.CardinalityReport
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: teamB}, &got); err != nil {
			return err
		}
		return wantCondition(got.Status.Conditions, condReady, metav1.ConditionTrue, "AnalysisSucceeded")
	})
	t.Logf("the report recovered %s after Grafana came back", time.Since(recovering).Round(time.Second))
}

// restartOperator restarts the operator, which clears its cached usage, and returns
// the time from which a report counts as fresh.
func restartOperator(t *testing.T) time.Time {
	t.Helper()
	since := time.Now().Truncate(time.Second)
	mustKubectl(t, "-n", operatorNamespace, "rollout", "restart", operatorDeploy)
	mustKubectl(t, "-n", operatorNamespace, "rollout", "status", operatorDeploy, "--timeout=2m")
	return since
}

// freshReport returns a Ready report generated after since.
func freshReport(ctx context.Context, name string, since time.Time) (*kardiganv1alpha1.CardinalityReport, error) {
	var report kardiganv1alpha1.CardinalityReport
	if err := k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: name}, &report); err != nil {
		return nil, err
	}
	if err := wantCondition(report.Status.Conditions, condReady, metav1.ConditionTrue, "AnalysisSucceeded"); err != nil {
		return nil, err
	}
	if report.Status.GeneratedAt == nil || report.Status.GeneratedAt.Time.Before(since) {
		return nil, fmt.Errorf("report was generated at %v, want after %v", report.Status.GeneratedAt, since)
	}
	return &report, nil
}

func hasAction(report *kardiganv1alpha1.CardinalityReport, action string) bool {
	return slices.ContainsFunc(report.Status.Recommendations, func(r kardiganv1alpha1.Recommendation) bool {
		return r.Action == action
	})
}

func testOutage(t *testing.T) {
	ctx := context.Background()
	cb := newBudget(testNamespace, "outage", "job", "avalanche-team-b")
	cb.Spec.Interval = metav1.Duration{Duration: 30 * time.Second}
	if err := k8s.Create(ctx, cb); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, cb) })

	var before kardiganv1alpha1.CardinalityBudget
	eventually(t, settleTimeout, func() error {
		if err := getBudget(t, testNamespace, "outage", &before); err != nil {
			return err
		}
		return wantCondition(before.Status.Conditions, condReady, metav1.ConditionTrue, "QuerySucceeded")
	})
	reportBefore := readyReport(t, testNamespace, "outage")

	docker(t, "stop", mimirContainer)
	started := false
	start := func() {
		if !started {
			docker(t, "start", mimirContainer)
			started = true
		}
	}
	t.Cleanup(start)

	// The budget's next check fails. It keeps its last good count, and the message
	// doesn't tell whoever can read the budget where Mimir is or what it said.
	eventually(t, settleTimeout, func() error {
		var got kardiganv1alpha1.CardinalityBudget
		if err := getBudget(t, testNamespace, "outage", &got); err != nil {
			return err
		}
		if err := wantCondition(got.Status.Conditions, condReady, metav1.ConditionFalse, "QueryFailed"); err != nil {
			return err
		}
		if got.Status.CurrentSeries != before.Status.CurrentSeries {
			t.Errorf("currentSeries = %d during the outage, want the last good %d",
				got.Status.CurrentSeries, before.Status.CurrentSeries)
		}
		wantNoUpstreamDetail(t, meta.FindStatusCondition(got.Status.Conditions, condReady).Message)
		return nil
	})

	// A spec edit makes the report analyse now, not on its hourly schedule. It fails
	// and keeps its last good recommendations.
	setMaxSeries(t, testNamespace, "outage", 3000)
	eventually(t, settleTimeout, func() error {
		var got kardiganv1alpha1.CardinalityReport
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "outage"}, &got); err != nil {
			return err
		}
		if err := wantCondition(got.Status.Conditions, condReady, metav1.ConditionFalse, "QueryFailed"); err != nil {
			return err
		}
		if len(got.Status.Recommendations) != len(reportBefore.Status.Recommendations) {
			t.Errorf("%d recommendations during the outage, want the last good %d",
				len(got.Status.Recommendations), len(reportBefore.Status.Recommendations))
		}
		wantNoUpstreamDetail(t, meta.FindStatusCondition(got.Status.Conditions, condReady).Message)
		return nil
	})

	start()

	// The budget retries every minute, and Mimir takes a moment to come back.
	eventually(t, reportTimeout, func() error {
		var got kardiganv1alpha1.CardinalityBudget
		if err := getBudget(t, testNamespace, "outage", &got); err != nil {
			return err
		}
		return wantCondition(got.Status.Conditions, condReady, metav1.ConditionTrue, "QuerySucceeded")
	})
	// The report retries every 5 minutes.
	eventually(t, 7*time.Minute, func() error {
		var got kardiganv1alpha1.CardinalityReport
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "outage"}, &got); err != nil {
			return err
		}
		return wantCondition(got.Status.Conditions, condReady, metav1.ConditionTrue, "AnalysisSucceeded")
	})
}

// grepKardigan keeps failure output readable: the endpoint has many other metrics.
func grepKardigan(body string) string {
	var lines []string
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "kardigan_") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// metricsReader returns a function that reads the operator's metrics endpoint. It
// goes through a port-forward with a service account token, as a scraper would.
func metricsReader(t *testing.T) func() string {
	t.Helper()
	const binding = "kardigan-stack-test-metrics"
	serviceAccount := operatorNamespace + ":kardigan-controller-manager"
	out, err := kubectl(t, "create", "clusterrolebinding", binding,
		"--clusterrole=kardigan-metrics-reader", "--serviceaccount="+serviceAccount)
	if err != nil && !strings.Contains(out, "already exists") {
		t.Fatalf("binding the metrics role: %v\n%s", err, out)
	}
	tokenOut, err := kubectl(t, "-n", operatorNamespace, "create", "token", "kardigan-controller-manager")
	if err != nil {
		t.Fatalf("creating a token: %v\n%s", err, tokenOut)
	}
	token := strings.TrimSpace(tokenOut)

	ctx := portForward(t, operatorNamespace, metricsService, metricsPort+":8443")

	// The certificate is self-signed unless --metrics-cert-path is set.
	httpClient := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	read := func() (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://localhost:"+metricsPort+"/metrics", nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := httpClient.Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("metrics endpoint returned HTTP %d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		return string(body), err
	}
	return func() string {
		t.Helper()
		var body string
		eventually(t, settleTimeout, func() error {
			var err error
			body, err = read()
			return err
		})
		return body
	}
}
