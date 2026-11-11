//go:build extended

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

package stack

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	kardiganv1alpha1 "github.com/pwgreco/kardigan/api/v1alpha1"
)

const (
	chartRelease   = "kardigan"
	chartDeploy    = "deploy/kardigan"
	chartService   = "kardigan-metrics"
	metricsAddress = "https://" + chartService + "." + operatorNamespace + ".svc:8443/metrics"

	curlImage = "curlimages/curl:8.17.0@sha256:935d9100e9ba842cdb060de42472c7ca90cfe9a7c96e4dacb55e79e560b3ff40"
	// curlTimedOut is curl's exit code when the connection never completes.
	curlTimedOut = 28

	operatorBundle = "https://github.com/prometheus-operator/prometheus-operator/releases/download/v0.94.2/bundle.yaml"
	teamOp         = "team-op"
	prometheusPort = "19090"
	prometheusURL  = "http://localhost:" + prometheusPort
	teamOpSamples  = 1000
)

// TestExtended runs the slower checks, against an operator installed from the chart.
func TestExtended(t *testing.T) {
	logOperatorOnFailure(t, chartDeploy)

	t.Run("the chart installs a working operator", testChartInstall)
	t.Run("the NetworkPolicy allows only the listed peers", testNetworkPolicy)
	t.Run("a report's snippet works through the Prometheus Operator", testSnippetOnScrape)
	t.Run("uninstalling the chart keeps CRDs and budgets", testChartUninstall)
}

func testChartInstall(t *testing.T) {
	mustKubectl(t, "apply", "-k", "../../config/samples/")
	eventually(t, settleTimeout, func() error {
		var cb kardiganv1alpha1.CardinalityBudget
		if err := getBudget(t, testNamespace, "team-a", &cb); err != nil {
			return err
		}
		return wantCondition(cb.Status.Conditions, condReady, metav1.ConditionTrue, "QuerySucceeded")
	})
	readyReport(t, testNamespace, "team-a")
}

func testNetworkPolicy(t *testing.T) {
	// Any HTTP answer, even a 401, means the connection got through.
	if code := probeMetrics(t, "metrics-allowed"); code != 0 {
		t.Errorf("a listed namespace can't reach the metrics port: curl exit code %d", code)
	}
	// A different failure, such as DNS, wouldn't show the policy blocked anything.
	if code := probeMetrics(t, "metrics-denied"); code != curlTimedOut {
		t.Errorf("an unlisted namespace: curl exit code %d, want %d, a timeout", code, curlTimedOut)
	}
}

// probeMetrics tries to reach the metrics port from a pod in namespace, and returns
// curl's exit code.
func probeMetrics(t *testing.T, namespace string) int32 {
	t.Helper()
	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := k8s.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "probe", Namespace: namespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "curl",
				Image: curlImage,
				Args:  []string{"-sk", "-o", "/dev/null", "--max-time", "10", metricsAddress},
			}},
		},
	}
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}

	var exitCode int32
	eventually(t, settleTimeout, func() error {
		var got corev1.Pod
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "probe"}, &got); err != nil {
			return err
		}
		for _, status := range got.Status.ContainerStatuses {
			if status.State.Terminated != nil {
				exitCode = status.State.Terminated.ExitCode
				return nil
			}
		}
		return fmt.Errorf("probe is %s", got.Status.Phase)
	})
	return exitCode
}

func testSnippetOnScrape(t *testing.T) {
	ctx := context.Background()
	mustKubectl(t, "create", "-f", operatorBundle)
	mustKubectl(t, "wait", "--for=condition=established", "--timeout=2m",
		"crd/prometheuses.monitoring.coreos.com", "crd/servicemonitors.monitoring.coreos.com")
	mustKubectl(t, "-n", testNamespace, "rollout", "status", "deploy/prometheus-operator", "--timeout=3m")

	eventually(t, settleTimeout, func() error {
		_, err := kubectl(t, "apply", "-f", "../../hack/kind/operator-test.yaml")
		return err
	})
	eventually(t, dataTimeout, func() error {
		_, err := kubectl(t, "-n", teamOp, "get", "pod/prometheus-team-op-0")
		return err
	})
	mustKubectl(t, "-n", teamOp, "wait", "--for=condition=Ready", "--timeout=5m", "pod/prometheus-team-op-0")
	waitForSeries(t, teamOp, teamOpSamples)

	queryCtx := portForward(t, teamOp, "pod/prometheus-team-op-0", prometheusPort+":9090")
	query := func(promql string) (float64, error) { return promQuery(queryCtx, prometheusURL, promql) }
	samples := `scrape_samples_post_metric_relabeling{namespace="team-op"}`
	eventually(t, settleTimeout, func() error {
		got, err := query(samples)
		if err != nil {
			return err
		}
		if got != teamOpSamples {
			return fmt.Errorf("scraped %v samples, want %d", got, teamOpSamples)
		}
		return nil
	})

	// The budget's first report may have run before the data arrived, and reports
	// are hourly, so a spec edit makes it analyse now.
	since := time.Now().Truncate(time.Second)
	setMaxSeries(t, teamOp, teamOp, 2500)
	var report kardiganv1alpha1.CardinalityReport
	eventually(t, reportTimeout, func() error {
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: teamOp, Name: teamOp}, &report); err != nil {
			return err
		}
		if err := wantCondition(report.Status.Conditions, condReady, metav1.ConditionTrue, "AnalysisSucceeded"); err != nil {
			return err
		}
		if report.Status.GeneratedAt == nil || report.Status.GeneratedAt.Time.Before(since) {
			return fmt.Errorf("report was generated at %v, want after %v", report.Status.GeneratedAt, since)
		}
		return nil
	})
	if report.Status.PotentialSavings <= 0 {
		t.Fatalf("potentialSavings = %d, so the snippet has nothing to prove", report.Status.PotentialSavings)
	}

	// A label the report says it can remove, and how many series that metric has now.
	var labelled kardiganv1alpha1.Recommendation
	for _, rec := range report.Status.Recommendations {
		if rec.Action == kardiganv1alpha1.ActionDropLabel {
			labelled = rec
			break
		}
	}
	if labelled.Label == "" {
		t.Fatalf("no DropLabel to test with: %v", report.Status.Recommendations)
	}
	series, err := query(fmt.Sprintf(`count({__name__=%q,namespace="team-op"})`, labelled.Metric))
	if err != nil {
		t.Fatal(err)
	}
	if series == 0 {
		t.Fatalf("no series for %s", labelled.Metric)
	}

	applySnippet(t, report.Status.RelabelSnippet)

	// What the report claimed: samples fall by exactly its savings.
	want := float64(teamOpSamples - report.Status.PotentialSavings)
	eventually(t, dataTimeout, func() error {
		got, err := query(samples)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("scraped %v samples, want %v", got, want)
		}
		return nil
	})

	// Removing the label must not merge series: they're all still there without it.
	eventually(t, settleTimeout, func() error {
		got, err := query(fmt.Sprintf(`count({__name__=%q,namespace="team-op",%s=""})`, labelled.Metric, labelled.Label))
		if err != nil {
			return err
		}
		if got != series {
			return fmt.Errorf("%s has %v series without %s, want %v", labelled.Metric, got, labelled.Label, series)
		}
		return nil
	})

	// The Operator's config generator could drop an empty replacement, which would
	// turn the rule into something that doesn't remove the label.
	if config := generatedPrometheusConfig(t); !strings.Contains(config, `replacement: ""`) {
		t.Errorf("generated config has no empty replacement:\n%s", config)
	}
}

// applySnippet appends a report's rules to the ServiceMonitor, after its own.
func applySnippet(t *testing.T, snippet string) {
	t.Helper()
	var parsed struct {
		MetricRelabelings []any `json:"metricRelabelings"`
	}
	if err := yaml.Unmarshal([]byte(snippet), &parsed); err != nil {
		t.Fatalf("the snippet isn't valid YAML: %v\n%s", err, snippet)
	}
	if len(parsed.MetricRelabelings) == 0 {
		t.Fatalf("the snippet has no rules:\n%s", snippet)
	}

	eventually(t, 30*time.Second, func() error {
		ctx := context.Background()
		monitor := &unstructured.Unstructured{}
		monitor.SetGroupVersionKind(schema.GroupVersionKind{
			Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor",
		})
		if err := k8s.Get(ctx, types.NamespacedName{Namespace: teamOp, Name: "avalanche"}, monitor); err != nil {
			return err
		}
		endpoints, _, err := unstructured.NestedSlice(monitor.Object, "spec", "endpoints")
		if err != nil || len(endpoints) != 1 {
			return fmt.Errorf("want one endpoint, got %v: %w", endpoints, err)
		}
		endpoint := endpoints[0].(map[string]any)
		rules, _ := endpoint["metricRelabelings"].([]any)
		endpoint["metricRelabelings"] = append(rules, parsed.MetricRelabelings...)
		if err := unstructured.SetNestedSlice(monitor.Object, endpoints, "spec", "endpoints"); err != nil {
			return err
		}
		return k8s.Update(ctx, monitor)
	})
}

func generatedPrometheusConfig(t *testing.T) string {
	t.Helper()
	var config string
	eventually(t, settleTimeout, func() error {
		var secret corev1.Secret
		key := types.NamespacedName{Namespace: teamOp, Name: "prometheus-team-op"}
		if err := k8s.Get(context.Background(), key, &secret); err != nil {
			return err
		}
		reader, err := gzip.NewReader(bytes.NewReader(secret.Data["prometheus.yaml.gz"]))
		if err != nil {
			return err
		}
		body, err := io.ReadAll(reader)
		config = string(body)
		return err
	})
	return config
}

func testChartUninstall(t *testing.T) {
	helm := os.Getenv("HELM")
	out, err := exec.CommandContext(context.Background(), helm, "uninstall", chartRelease,
		"--namespace", operatorNamespace, "--wait").CombinedOutput()
	if err != nil {
		t.Fatalf("helm uninstall: %v\n%s", err, out)
	}
	mustKubectl(t, "get", "crd", "cardinalitybudgets.kardigan.paulgreco.ca", "cardinalityreports.kardigan.paulgreco.ca")
	var cb kardiganv1alpha1.CardinalityBudget
	if err := getBudget(t, testNamespace, "team-a", &cb); err != nil {
		t.Errorf("the budget should survive an uninstall: %v", err)
	}
}
