//go:build stack || extended

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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	kardiganv1alpha1 "github.com/pwgreco/kardigan/api/v1alpha1"
	"github.com/pwgreco/kardigan/internal/mimir"
)

const (
	operatorNamespace = "kardigan-system"
	testNamespace     = "default"
	mimirContainer    = "kardigan-mimir"
	grafanaContainer  = "kardigan-grafana"
	mimirFromHost     = "http://localhost:9009"

	condReady        = "Ready"
	condWithinBudget = "WithinBudget"
	condUsage        = "UsageComplete"

	settleTimeout = 2 * time.Minute
	// reportTimeout covers one analysis per budget, run one query at a time.
	reportTimeout = 5 * time.Minute
	// dataTimeout covers pulling images, starting pods and the first remote writes.
	dataTimeout = 6 * time.Minute

	namespaceLabel = "namespace"
)

var k8s client.Client

func TestMain(m *testing.M) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		panic(err)
	}
	if err := kardiganv1alpha1.AddToScheme(scheme); err != nil {
		panic(err)
	}
	cfg, err := config.GetConfig()
	if err != nil {
		panic(err)
	}
	if k8s, err = client.New(cfg, client.Options{Scheme: scheme}); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// wantNoUpstreamDetail fails if a status message reveals Mimir's address.
func wantNoUpstreamDetail(t *testing.T, message string) {
	t.Helper()
	for _, leak := range []string{mimirContainer, grafanaContainer, "http://", "dial tcp", "lookup "} {
		if strings.Contains(message, leak) {
			t.Errorf("status message %q contains %q", message, leak)
		}
	}
}

func newBudget(namespace, name, label, value string) *kardiganv1alpha1.CardinalityBudget {
	return &kardiganv1alpha1.CardinalityBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: kardiganv1alpha1.CardinalityBudgetSpec{
			Target:    kardiganv1alpha1.BudgetTarget{Label: label, Value: value},
			MaxSeries: 100000,
		},
	}
}

func getBudget(t *testing.T, namespace, name string, cb *kardiganv1alpha1.CardinalityBudget) error {
	t.Helper()
	return k8s.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, cb)
}

func setMaxSeries(t *testing.T, namespace, name string, maxSeries int64) {
	t.Helper()
	eventually(t, 30*time.Second, func() error {
		var cb kardiganv1alpha1.CardinalityBudget
		if err := getBudget(t, namespace, name, &cb); err != nil {
			return err
		}
		cb.Spec.MaxSeries = maxSeries
		return k8s.Update(context.Background(), &cb)
	})
}

// readyReport waits for a budget's report to finish an analysis.
func readyReport(t *testing.T, namespace, name string) *kardiganv1alpha1.CardinalityReport {
	t.Helper()
	var report kardiganv1alpha1.CardinalityReport
	eventually(t, reportTimeout, func() error {
		key := types.NamespacedName{Namespace: namespace, Name: name}
		if err := k8s.Get(context.Background(), key, &report); err != nil {
			return err
		}
		return wantCondition(report.Status.Conditions, condReady, metav1.ConditionTrue, "AnalysisSucceeded")
	})
	return &report
}

func wantCondition(conditions []metav1.Condition, condType string, status metav1.ConditionStatus, reason string) error {
	cond := meta.FindStatusCondition(conditions, condType)
	if cond == nil {
		return fmt.Errorf("no %s condition in %+v", condType, conditions)
	}
	if cond.Status != status || cond.Reason != reason {
		return fmt.Errorf("%s = %s/%s (%s), want %s/%s", condType, cond.Status, cond.Reason, cond.Message, status, reason)
	}
	return nil
}

// waitForSeries waits until Mimir has at least want active series for a namespace,
// so that a budget's first check, which sets its schedule, sees real data.
func waitForSeries(t *testing.T, namespace string, want int64) {
	t.Helper()
	mimirClient := &mimir.Client{BaseURL: mimirFromHost, Tenant: "demo", HTTP: &http.Client{Timeout: 10 * time.Second}}
	eventually(t, dataTimeout, func() error {
		got, err := mimirClient.CountSeries(context.Background(), namespaceLabel, namespace)
		if err != nil {
			return err
		}
		if got < want {
			return fmt.Errorf("namespace %s has %d series, want at least %d", namespace, got, want)
		}
		return nil
	})
}

// portForward forwards a local port for the rest of the test. It returns a context
// that ends with the test.
func portForward(t *testing.T, namespace, target, ports string) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	forward := exec.CommandContext(ctx, "kubectl", "-n", namespace, "port-forward", target, ports)
	if err := forward.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		_ = forward.Wait()
	})
	return ctx
}

// promQuery runs an instant query that should return one number. An empty result,
// which is how count() reports no series, is zero.
func promQuery(ctx context.Context, baseURL, promql string) (float64, error) {
	endpoint := baseURL + "/api/v1/query?query=" + url.QueryEscape(promql)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("query %s: HTTP %d: %s", promql, resp.StatusCode, body)
	}
	var out struct {
		Data struct {
			Result []struct {
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, err
	}
	if len(out.Data.Result) == 0 {
		return 0, nil
	}
	if len(out.Data.Result) != 1 || len(out.Data.Result[0].Value) != 2 {
		return 0, fmt.Errorf("query %s: want one sample, got %s", promql, body)
	}
	text, ok := out.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("query %s: malformed value in %s", promql, body)
	}
	return strconv.ParseFloat(text, 64)
}

func mustKubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := kubectl(t, args...)
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// logOperatorOnFailure prints the operator's log if the test fails.
func logOperatorOnFailure(t *testing.T, deployment string) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := kubectl(t, "-n", operatorNamespace, "logs", deployment, "--tail=100")
			t.Logf("operator log:\n%s", out)
		}
	})
}

func eventually(t *testing.T, timeout time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still failing after %s: %v", timeout, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func kubectl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "kubectl", args...).CombinedOutput()
	return string(out), err
}

func docker(t *testing.T, args ...string) {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
}
