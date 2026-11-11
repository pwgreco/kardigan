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
	"errors"
	"fmt"
	"strings"
	"testing"

	kardiganv1alpha1 "github.com/pwgreco/kardigan/api/v1alpha1"
	"github.com/pwgreco/kardigan/internal/mimir"
	"github.com/pwgreco/kardigan/internal/usage"
)

const (
	testLabel = "job"
	testValue = "team"

	bigMetric   = "big_total"
	usedMetric  = "used_total"
	otherMetric = "other_total"
	podLabel    = "pod"
)

// fakeSource stands in for Mimir. counts maps a PromQL query to its result.
type fakeSource struct {
	metrics []mimir.NameCount
	total   int64
	labels  map[string][]mimir.NameCount // metric -> labels
	counts  map[string]int64
	err     error
	queries int
	// labelGone means Mimir's label names list labels that queries no longer find.
	labelGone bool
}

func (f *fakeSource) SeriesPerMetric(context.Context, string, string) ([]mimir.NameCount, int64, error) {
	return f.metrics, f.total, f.err
}

func (f *fakeSource) LabelValueCounts(_ context.Context, selector string) ([]mimir.NameCount, error) {
	for metric, labels := range f.labels {
		if selector == mimir.MetricSelector(metric, testLabel, testValue) {
			return labels, nil
		}
	}
	return nil, nil
}

func (f *fakeSource) QueryCount(_ context.Context, promql string) (int64, error) {
	f.queries++
	if f.err != nil {
		return 0, f.err
	}
	if strings.HasPrefix(promql, "count(count by (") {
		if f.labelGone {
			return 1, nil
		}
		return 1000, nil
	}
	n, ok := f.counts[promql]
	if !ok {
		return 0, fmt.Errorf("unexpected query %s", promql)
	}
	return n, nil
}

func before(metric string) string {
	return fmt.Sprintf("count(%s)", mimir.MetricSelector(metric, testLabel, testValue))
}

func after(metric string) string {
	return fmt.Sprintf("count(count without (%s) (%s))", podLabel, mimir.MetricSelector(metric, testLabel, testValue))
}

func snapshotOf(t *testing.T, sources map[string][]string) *usage.Snapshot {
	t.Helper()
	list := make([]usage.Source, 0, len(sources))
	for name, exprs := range sources {
		list = append(list, usage.Source{Name: name, Fetch: func(context.Context) ([]string, error) {
			return exprs, nil
		}})
	}
	snapshot, err := usage.Collect(context.Background(), list)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func dashboards(t *testing.T, exprs ...string) *usage.Snapshot {
	t.Helper()
	return snapshotOf(t, map[string][]string{DashboardSource: exprs})
}

func analyze(t *testing.T, src *fakeSource, snapshot *usage.Snapshot) Result {
	t.Helper()
	result, err := Analyze(context.Background(), src, testLabel, testValue, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func find(result Result, action, metric, label string) *kardiganv1alpha1.Recommendation {
	for i, rec := range result.Recommendations {
		if rec.Action == action && rec.Metric == metric && rec.Label == label {
			return &result.Recommendations[i]
		}
	}
	return nil
}

func TestDropMetricConfidence(t *testing.T) {
	src := &fakeSource{metrics: []mimir.NameCount{{Name: bigMetric, Count: 500}}, total: 500}

	tests := []struct {
		name       string
		snapshot   *usage.Snapshot
		confidence string
		reason     string
	}{
		{"nothing reads it", dashboards(t, otherMetric), kardiganv1alpha1.ConfidenceMedium, "Ad hoc queries"},
		{"a regex could", dashboards(t, `{__name__=~"big_.*"}`), kardiganv1alpha1.ConfidenceLow, "regex"},
		{"a variable could", dashboards(t, "big_"+usage.Placeholder), kardiganv1alpha1.ConfidenceLow, "regex"},
		{"an expression failed to parse", dashboards(t, otherMetric, "rate("), kardiganv1alpha1.ConfidenceLow, "1 expressions"},
		{"dashboards not checked", snapshotOf(t, map[string][]string{"rules": {otherMetric}}),
			kardiganv1alpha1.ConfidenceLow, "Dashboards weren't checked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := find(analyze(t, src, tt.snapshot), kardiganv1alpha1.ActionDropMetric, bigMetric, "")
			if rec == nil {
				t.Fatal("want a DropMetric")
			}
			if rec.SeriesSaved != 500 {
				t.Errorf("SeriesSaved = %d", rec.SeriesSaved)
			}
			if rec.Confidence != tt.confidence {
				t.Errorf("Confidence = %s, want %s", rec.Confidence, tt.confidence)
			}
			if !strings.Contains(rec.Reason, tt.reason) {
				t.Errorf("Reason = %q, want it to mention %q", rec.Reason, tt.reason)
			}
		})
	}
}

func TestUsedMetricIsKept(t *testing.T) {
	src := &fakeSource{metrics: []mimir.NameCount{{Name: usedMetric, Count: 50}}, total: 50}
	result := analyze(t, src, dashboards(t, usedMetric))

	if find(result, kardiganv1alpha1.ActionKeep, usedMetric, "") == nil {
		t.Errorf("want Keep, got %+v", result.Recommendations)
	}
	if got := result.PotentialSavings(); got != 0 {
		t.Errorf("PotentialSavings() = %d", got)
	}
}

func TestGeneratedMetricsAreSkipped(t *testing.T) {
	src := &fakeSource{metrics: []mimir.NameCount{{Name: "up", Count: 1}, {Name: "scrape_series_added", Count: 1}}}
	if result := analyze(t, src, dashboards(t, otherMetric)); len(result.Recommendations) != 0 {
		t.Errorf("metricRelabelings can't drop these, got %+v", result.Recommendations)
	}
}

// The core rule: labeldrop-style removal only appears when no series would merge.
func TestLabelUniqueness(t *testing.T) {
	labels := map[string][]mimir.NameCount{usedMetric: {{Name: podLabel, Count: 100}}}
	metrics := []mimir.NameCount{{Name: usedMetric, Count: 1000}}

	t.Run("series stay distinct", func(t *testing.T) {
		src := &fakeSource{metrics: metrics, labels: labels, counts: map[string]int64{
			before(usedMetric): 1000, after(usedMetric): 1000,
		}}
		result := analyze(t, src, dashboards(t, usedMetric))

		rec := find(result, kardiganv1alpha1.ActionDropLabel, usedMetric, podLabel)
		if rec == nil {
			t.Fatalf("want DropLabel, got %+v", result.Recommendations)
		}
		if rec.SeriesSaved != 0 {
			t.Errorf("SeriesSaved = %d: if every series survives, none are saved", rec.SeriesSaved)
		}
	})

	t.Run("series would merge", func(t *testing.T) {
		src := &fakeSource{metrics: metrics, labels: labels, counts: map[string]int64{
			before(usedMetric): 1000, after(usedMetric): 10,
		}}
		result := analyze(t, src, dashboards(t, usedMetric))

		if find(result, kardiganv1alpha1.ActionDropLabel, usedMetric, podLabel) != nil {
			t.Error("must not suggest DropLabel when series would merge")
		}
		rec := find(result, kardiganv1alpha1.ActionFixLabel, usedMetric, podLabel)
		if rec == nil {
			t.Fatalf("want FixLabel, got %+v", result.Recommendations)
		}
		if rec.SeriesSaved != 990 {
			t.Errorf("SeriesSaved = %d, want 990", rec.SeriesSaved)
		}
		if got := result.PotentialSavings(); got != 0 {
			t.Errorf("PotentialSavings() = %d, but the snippet can't make this change", got)
		}
	})

	t.Run("samples have stopped but Mimir still counts the metric as active", func(t *testing.T) {
		src := &fakeSource{metrics: metrics, labels: labels, counts: map[string]int64{
			before(usedMetric): 0, after(usedMetric): 0,
		}}
		result := analyze(t, src, dashboards(t, usedMetric))
		if len(result.Recommendations) != 1 {
			t.Errorf("no series means the check says nothing, got %+v", result.Recommendations)
		}
	})

	t.Run("series changed between the queries", func(t *testing.T) {
		src := &fakeSource{metrics: metrics, labels: labels, counts: map[string]int64{
			before(usedMetric): 1000, after(usedMetric): 1001,
		}}
		result := analyze(t, src, dashboards(t, usedMetric))
		if len(result.Recommendations) != 1 {
			t.Errorf("want only Keep, got %+v", result.Recommendations)
		}
	})
}

// Two labels that copy each other each pass alone, but removing both merges every
// series. Only one may reach the snippet.
func TestRedundantLabelsAreCheckedTogether(t *testing.T) {
	const copyLabel = "pod_uid"
	selector := mimir.MetricSelector(usedMetric, testLabel, testValue)
	src := &fakeSource{
		metrics: []mimir.NameCount{{Name: usedMetric, Count: 1000}},
		labels:  map[string][]mimir.NameCount{usedMetric: {{Name: copyLabel, Count: 100}, {Name: podLabel, Count: 100}}},
		counts: map[string]int64{
			before(usedMetric): 1000,
			fmt.Sprintf("count(count without (%s) (%s))", copyLabel, selector):               1000,
			fmt.Sprintf("count(count without (%s, %s) (%s))", copyLabel, podLabel, selector): 10,
		},
	}
	result := analyze(t, src, dashboards(t, usedMetric))

	if find(result, kardiganv1alpha1.ActionDropLabel, usedMetric, copyLabel) == nil {
		t.Errorf("want DropLabel on the first label, got %+v", result.Recommendations)
	}
	if find(result, kardiganv1alpha1.ActionDropLabel, usedMetric, podLabel) != nil {
		t.Error("must not suggest removing both labels")
	}
	rec := find(result, kardiganv1alpha1.ActionFixLabel, usedMetric, podLabel)
	if rec == nil {
		t.Fatalf("want FixLabel on the second label, got %+v", result.Recommendations)
	}
	if rec.SeriesSaved != 990 || !strings.Contains(rec.Reason, "Without it and "+copyLabel) {
		t.Errorf("got %+v", rec)
	}

	snippet, err := Snippet("team", generated, result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snippet, "targetLabel: "+podLabel+"\n") {
		t.Errorf("the snippet removes both labels:\n%s", snippet)
	}
}

func TestLabelsLeftAlone(t *testing.T) {
	const noisy = 1000
	tests := []struct {
		name  string
		label string
		exprs []string
	}{
		{"used in a matcher", podLabel, []string{`used_total{pod="x"}`}},
		{"grouped on elsewhere", podLabel, []string{usedMetric, `sum by (pod) (other_total)`}},
		{"grouping on a variable", podLabel, []string{usedMetric, `sum by (` + usage.Placeholder + `) (other_total)`}},
		{"used through a regex selector", podLabel, []string{usedMetric, `{__name__=~"o.*", pod="x"}`}},
		{"job identifies the target", "job", []string{usedMetric}},
		{"instance identifies the target", "instance", []string{usedMetric}},
		{"le carries meaning", bucketLabel, []string{usedMetric}},
		{"quantile carries meaning", "quantile", []string{usedMetric}},
		{"name", "__name__", []string{usedMetric}},
		{"name that PromQL would need quoted", "http.method", []string{usedMetric}},
		{"name trying to change the query", "pod) or (up", []string{usedMetric}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSource{
				metrics: []mimir.NameCount{{Name: usedMetric, Count: 1000}},
				labels:  map[string][]mimir.NameCount{usedMetric: {{Name: tt.label, Count: noisy}}},
			}
			result := analyze(t, src, dashboards(t, tt.exprs...))
			if src.queries != 0 || len(result.Recommendations) != 1 {
				t.Errorf("queries = %d, recommendations = %+v", src.queries, result.Recommendations)
			}
		})
	}
}

func TestLabelWithFewValuesIsIgnored(t *testing.T) {
	src := &fakeSource{
		metrics: []mimir.NameCount{{Name: usedMetric, Count: 1000}},
		labels:  map[string][]mimir.NameCount{usedMetric: {{Name: podLabel, Count: minLabelValues - 1}}},
	}
	analyze(t, src, dashboards(t, usedMetric))
	if src.queries != 0 {
		t.Errorf("queries = %d, want none", src.queries)
	}
}

// After a snippet removes a label, Mimir's label names keep listing it for hours.
func TestLabelAlreadyRemovedIsIgnored(t *testing.T) {
	src := &fakeSource{
		metrics:   []mimir.NameCount{{Name: usedMetric, Count: 1000}},
		labels:    map[string][]mimir.NameCount{usedMetric: {{Name: podLabel, Count: 100}}},
		labelGone: true,
	}
	result := analyze(t, src, dashboards(t, usedMetric))
	if len(result.Recommendations) != 1 {
		t.Errorf("want only Keep, got %+v", result.Recommendations)
	}
}

func TestLabelChecksAreLimited(t *testing.T) {
	src := &fakeSource{labels: map[string][]mimir.NameCount{}, counts: map[string]int64{}}
	exprs := make([]string, 0, 30)
	for i := range maxLabelChecks + 5 {
		name := fmt.Sprintf("m%02d_total", i)
		src.metrics = append(src.metrics, mimir.NameCount{Name: name, Count: 100})
		src.labels[name] = []mimir.NameCount{{Name: podLabel, Count: 100}}
		src.counts[before(name)], src.counts[after(name)] = 100, 100
		exprs = append(exprs, name)
	}
	analyze(t, src, dashboards(t, exprs...))
	if want := maxLabelChecks * 3; src.queries != want {
		t.Errorf("queries = %d, want %d", src.queries, want)
	}
}

func TestTrimHistogram(t *testing.T) {
	const metric = "latency_bucket"
	tests := []struct {
		name    string
		metric  string
		buckets int64
		want    bool
		saved   int64
	}{
		{"too many buckets", metric, 30, true, 500},
		{"few buckets", metric, maxBuckets, false, 0},
		{"le on something else", "weird_total", 30, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSource{
				metrics: []mimir.NameCount{{Name: tt.metric, Count: 1000}},
				labels:  map[string][]mimir.NameCount{tt.metric: {{Name: bucketLabel, Count: tt.buckets}}},
			}
			rec := find(analyze(t, src, dashboards(t, tt.metric)), kardiganv1alpha1.ActionTrimHistogram, tt.metric, "")
			if (rec != nil) != tt.want {
				t.Fatalf("TrimHistogram = %+v, want %v", rec, tt.want)
			}
			if rec != nil && rec.SeriesSaved != tt.saved {
				t.Errorf("SeriesSaved = %d, want %d", rec.SeriesSaved, tt.saved)
			}
		})
	}
}

func TestLimit(t *testing.T) {
	src := &fakeSource{}
	exprs := make([]string, 0, 30)
	// Largest first, as Mimir's results are sorted.
	for i := range 40 {
		src.metrics = append(src.metrics, mimir.NameCount{Name: fmt.Sprintf("unused_%02d", i), Count: int64(1000 - i)})
	}
	for i := range 10 {
		name := fmt.Sprintf("used_%02d", i)
		src.metrics = append(src.metrics, mimir.NameCount{Name: name, Count: 10})
		exprs = append(exprs, name)
	}
	result := analyze(t, src, dashboards(t, exprs...))

	if got := len(result.Recommendations); got != maxRecommendations {
		t.Fatalf("got %d recommendations, want %d", got, maxRecommendations)
	}
	drops, keeps := 0, 0
	for _, rec := range result.Recommendations {
		switch rec.Action {
		case kardiganv1alpha1.ActionDropMetric:
			drops++
		case kardiganv1alpha1.ActionKeep:
			keeps++
		}
	}
	if drops != maxRecommendations-maxKeep || keeps != maxKeep {
		t.Errorf("drops = %d, keeps = %d", drops, keeps)
	}
	if first := result.Recommendations[0]; first.Metric != "unused_00" {
		t.Errorf("first = %s, want the largest", first.Metric)
	}
	if find(result, kardiganv1alpha1.ActionDropMetric, "unused_39", "") != nil {
		t.Error("the smallest savings should be cut")
	}
}

func TestLimitLeavesRoomUnused(t *testing.T) {
	src := &fakeSource{}
	exprs := make([]string, 0, 30)
	for i := range 3 {
		src.metrics = append(src.metrics, mimir.NameCount{Name: fmt.Sprintf("unused_%d", i), Count: 10})
	}
	for i := range 30 {
		name := fmt.Sprintf("used_%02d", i)
		src.metrics = append(src.metrics, mimir.NameCount{Name: name, Count: 10})
		exprs = append(exprs, name)
	}
	result := analyze(t, src, dashboards(t, exprs...))
	if got := len(result.Recommendations); got != maxRecommendations {
		t.Errorf("got %d, want Keep entries to fill the room actions don't use", got)
	}
}

func TestDeterministic(t *testing.T) {
	src := &fakeSource{metrics: []mimir.NameCount{
		{Name: "b_total", Count: 5}, {Name: "a_total", Count: 5}, {Name: "c_total", Count: 5},
	}}
	result := analyze(t, src, dashboards(t, otherMetric))
	names := make([]string, 0, len(result.Recommendations))
	for _, rec := range result.Recommendations {
		names = append(names, rec.Metric)
	}
	if got := strings.Join(names, ","); got != "a_total,b_total,c_total" {
		t.Errorf("order = %s", got)
	}
}

func TestErrorsAreReturned(t *testing.T) {
	boom := errors.New("boom")

	t.Run("listing metrics", func(t *testing.T) {
		_, err := Analyze(context.Background(), &fakeSource{err: boom}, testLabel, testValue, dashboards(t, otherMetric))
		if !errors.Is(err, boom) {
			t.Errorf("err = %v", err)
		}
	})

	// A failed uniqueness query must never read as "safe to drop".
	t.Run("uniqueness check", func(t *testing.T) {
		src := &failingQueries{fakeSource: fakeSource{
			metrics: []mimir.NameCount{{Name: usedMetric, Count: 100}},
			labels:  map[string][]mimir.NameCount{usedMetric: {{Name: podLabel, Count: 100}}},
		}, err: boom}
		_, err := Analyze(context.Background(), src, testLabel, testValue, dashboards(t, usedMetric))
		if !errors.Is(err, boom) {
			t.Errorf("err = %v", err)
		}
	})
}

type failingQueries struct {
	fakeSource
	err error
}

func (f *failingQueries) QueryCount(context.Context, string) (int64, error) {
	return 0, f.err
}
