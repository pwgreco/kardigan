//go:build integration

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

package recommend

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/pwgreco/kardigan/internal/grafana"
	"github.com/pwgreco/kardigan/internal/mimir"
	"github.com/pwgreco/kardigan/internal/usage"
)

// mimirtoolResult is the part of mimirtool's prometheus-metrics.json compared here.
type mimirtoolResult struct {
	InUse      []mimirtoolMetric `json:"in_use_metric_counts"`
	Additional []mimirtoolMetric `json:"additional_metric_counts"`
}

type mimirtoolMetric struct {
	Metric    string `json:"metric"`
	JobCounts []struct {
		Job   string `json:"job"`
		Count int64  `json:"count"`
	} `json:"job_counts"`
}

// TestAgainstMimirtool compares every metric of every job with mimirtool's verdict,
// bypassing the report's cap of 25 entries. It reads the same dashboards and rules
// as mimirtool, so PrometheusRule objects aren't included. mimirtool only has used
// or unused verdicts, so this says nothing about label or histogram advice.
//
// Run it with make test-integration, which starts the local stack and runs mimirtool.
func TestAgainstMimirtool(t *testing.T) {
	path := os.Getenv("KARDIGAN_MIMIRTOOL_METRICS")
	if path == "" {
		t.Skip("KARDIGAN_MIMIRTOOL_METRICS not set")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want mimirtoolResult
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	httpClient := &http.Client{Timeout: 30 * time.Second}
	mimirClient := &mimir.Client{BaseURL: os.Getenv("KARDIGAN_MIMIR_URL"), Tenant: "demo", HTTP: httpClient}
	grafanaClient := &grafana.Client{BaseURL: os.Getenv("KARDIGAN_GRAFANA_URL"), HTTP: httpClient}
	snapshot, err := usage.Collect(ctx, []usage.Source{
		{Name: DashboardSource, Fetch: grafanaClient.Expressions},
		{Name: "mimir-ruler", Fetch: mimirClient.RuleExpressions},
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ParseFailures > 0 {
		t.Errorf("%d expressions failed to parse", snapshot.ParseFailures)
	}

	jobs := map[string]map[string]int64{} // job -> metric -> series, as Kardigan sees them
	seriesFor := func(job string) map[string]int64 {
		if jobs[job] == nil {
			metrics, _, err := mimirClient.SeriesPerMetric(ctx, "job", job)
			if err != nil {
				t.Fatal(err)
			}
			jobs[job] = map[string]int64{}
			for _, m := range metrics {
				jobs[job][m.Name] = m.Count
			}
		}
		return jobs[job]
	}

	compare := func(metrics []mimirtoolMetric, used bool) (agreed, generated int) {
		for _, m := range metrics {
			if generatedMetrics[m.Metric] {
				generated++ // Kardigan skips these, because relabelling can't drop them.
				continue
			}
			if got := snapshot.Usage.MetricUsed(m.Metric); got != used {
				t.Errorf("%s: Kardigan says used = %v, mimirtool says %v", m.Metric, got, used)
				continue
			}
			agreed++
			for _, jc := range m.JobCounts {
				if jc.Job == "" {
					continue
				}
				if got := seriesFor(jc.Job)[m.Metric]; got != jc.Count {
					t.Errorf("%s in %s: Kardigan counts %d series, mimirtool %d", m.Metric, jc.Job, got, jc.Count)
				}
			}
		}
		return agreed, generated
	}

	usedAgreed, usedGenerated := compare(want.InUse, true)
	unusedAgreed, unusedGenerated := compare(want.Additional, false)
	t.Logf("agree on %d used and %d unused metrics, across %d jobs; skipped %d generated",
		usedAgreed, unusedAgreed, len(jobs), usedGenerated+unusedGenerated)
}
