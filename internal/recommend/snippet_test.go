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
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/relabel"
	"sigs.k8s.io/yaml"

	kardiganv1alpha1 "github.com/pwgreco/kardigan/api/v1alpha1"
)

const keptMetric = "kept_total"

var generated = time.Date(2026, time.December, 1, 0, 0, 0, 0, time.UTC)

func sampleResult() Result {
	return Result{Recommendations: []kardiganv1alpha1.Recommendation{
		{Action: kardiganv1alpha1.ActionDropMetric, Metric: "b_total", SeriesSaved: 200},
		{Action: kardiganv1alpha1.ActionDropMetric, Metric: "a.total", SeriesSaved: 100},
		{Action: kardiganv1alpha1.ActionDropLabel, Metric: keptMetric, Label: "pod"},
		{Action: kardiganv1alpha1.ActionFixLabel, Metric: keptMetric, Label: "user", SeriesSaved: 900},
		{Action: kardiganv1alpha1.ActionTrimHistogram, Metric: "latency_bucket", SeriesSaved: 400},
		{Action: kardiganv1alpha1.ActionKeep, Metric: keptMetric},
	}}
}

func TestSnippet(t *testing.T) {
	got, err := Snippet("team-c", generated, sampleResult())
	if err != nil {
		t.Fatal(err)
	}
	want := `# Kardigan recommendations for team-c, 2026-12-01. Saves ~300 series.
metricRelabelings:
- action: drop
  regex: a\.total|b_total
  sourceLabels:
  - __name__
- action: replace
  regex: kept_total
  replacement: ""
  sourceLabels:
  - __name__
  targetLabel: pod
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "labeldrop") {
		t.Error("labeldrop can't be limited to one metric")
	}
}

func TestSnippetEmpty(t *testing.T) {
	result := Result{Recommendations: []kardiganv1alpha1.Recommendation{
		{Action: kardiganv1alpha1.ActionKeep, Metric: keptMetric},
		{Action: kardiganv1alpha1.ActionFixLabel, Metric: keptMetric, Label: "user", SeriesSaved: 900},
	}}
	got, err := Snippet("team-c", generated, result)
	if err != nil || got != "" {
		t.Errorf("got %q, err = %v", got, err)
	}
}

// TestSnippetInPrometheus runs the generated rules through Prometheus's own
// relabelling code, to prove they do what the report claims.
func TestSnippetInPrometheus(t *testing.T) {
	text, err := Snippet("team-c", generated, sampleResult())
	if err != nil {
		t.Fatal(err)
	}
	rules := parseRules(t, text)

	tests := []struct {
		name   string
		series labels.Labels
		want   labels.Labels // empty means dropped
	}{
		{
			name:   "unused metric is dropped",
			series: labels.FromStrings(model.MetricNameLabel, "b_total", "pod", "x"),
		},
		{
			name:   "a metric name's dot is matched literally",
			series: labels.FromStrings(model.MetricNameLabel, "a.total"),
		},
		{
			name:   "a metric differing only by the escaped character is kept",
			series: labels.FromStrings(model.MetricNameLabel, "axtotal"),
			want:   labels.FromStrings(model.MetricNameLabel, "axtotal"),
		},
		{
			name:   "a metric merely containing a name is kept, because regexes are anchored",
			series: labels.FromStrings(model.MetricNameLabel, "xb_total"),
			want:   labels.FromStrings(model.MetricNameLabel, "xb_total"),
		},
		{
			name:   "label is removed from the metric concerned",
			series: labels.FromStrings(model.MetricNameLabel, keptMetric, "pod", "x", "node", "y"),
			want:   labels.FromStrings(model.MetricNameLabel, keptMetric, "node", "y"),
		},
		{
			name:   "label stays on every other metric",
			series: labels.FromStrings(model.MetricNameLabel, "other_total", "pod", "x"),
			want:   labels.FromStrings(model.MetricNameLabel, "other_total", "pod", "x"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := labels.NewBuilder(tt.series)
			keep := relabel.ProcessBuilder(builder, rules...)
			if keep != !tt.want.IsEmpty() {
				t.Fatalf("keep = %v", keep)
			}
			if got := builder.Labels(); keep && !labels.Equal(got, tt.want) {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func parseRules(t *testing.T, text string) []*relabel.Config {
	t.Helper()
	var parsed struct {
		MetricRelabelings []relabelRule `json:"metricRelabelings"`
	}
	if err := yaml.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatal(err)
	}

	rules := make([]*relabel.Config, 0, len(parsed.MetricRelabelings))
	for _, rule := range parsed.MetricRelabelings {
		regex, err := relabel.NewRegexp(rule.Regex)
		if err != nil {
			t.Fatal(err)
		}
		cfg := relabel.DefaultRelabelConfig
		cfg.Regex = regex
		cfg.Action = relabel.Action(rule.Action)
		cfg.TargetLabel = rule.TargetLabel
		cfg.SourceLabels = model.LabelNames{}
		for _, source := range rule.SourceLabels {
			cfg.SourceLabels = append(cfg.SourceLabels, model.LabelName(source))
		}
		if rule.Replacement != nil {
			cfg.Replacement = *rule.Replacement
		}
		cfg.NameValidationScheme = model.UTF8Validation
		if err := cfg.Validate(model.UTF8Validation); err != nil {
			t.Fatalf("Prometheus rejects rule %+v: %v", rule, err)
		}
		rules = append(rules, &cfg)
	}
	return rules
}
