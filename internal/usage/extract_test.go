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

package usage

import "testing"

const (
	aTotal    = "a_total"
	httpTotal = "http_requests_total"
	jobLabel  = "job"
	podLabel  = "pod"
	nodeCPU   = "node_cpu"
	anything  = "anything"
)

func TestAdd(t *testing.T) {
	type label struct{ metric, name string }

	tests := []struct {
		name         string
		expr         string
		metrics      []string // MetricUsed is true
		notMetrics   []string // MetricUsed is false
		couldMatch   []string
		noMatch      []string
		labels       []label // LabelUsed is true
		unusedLabels []label
	}{
		{
			name:         "plain rate",
			expr:         `rate(http_requests_total{job="api"}[5m])`,
			metrics:      []string{httpTotal},
			notMetrics:   []string{"api"},
			labels:       []label{{httpTotal, jobLabel}},
			unusedLabels: []label{{httpTotal, podLabel}, {"other_metric", jobLabel}},
		},
		{
			name:    "aggregation by",
			expr:    `sum by (namespace) (rate(a_total[5m]))`,
			metrics: []string{aTotal},
			labels:  []label{{aTotal, "namespace"}, {"unrelated_metric", "namespace"}},
		},
		{
			name:    "aggregation without",
			expr:    `sum without (pod) (a_total)`,
			metrics: []string{aTotal},
			labels:  []label{{aTotal, podLabel}},
		},
		{
			name:    "histogram quantile",
			expr:    `histogram_quantile(0.9, sum by (le) (rate(latency_bucket[5m])))`,
			metrics: []string{"latency_bucket"},
			labels:  []label{{"latency_bucket", "le"}},
		},
		{
			name:    "join",
			expr:    `a_total * on (pod) group_left (node) b_info`,
			metrics: []string{aTotal, "b_info"},
			labels:  []label{{aTotal, podLabel}, {"b_info", "node"}},
		},
		{
			name:    "name given as a matcher",
			expr:    `{__name__="a_total", job="api"}`,
			metrics: []string{aTotal},
			labels:  []label{{aTotal, jobLabel}},
		},
		{
			name:       "regex name can't be listed but protects its labels",
			expr:       `{__name__=~"node_.*", device="sda"}`,
			notMetrics: []string{nodeCPU},
			couldMatch: []string{nodeCPU},
			noMatch:    []string{"xnode_cpu", httpTotal},
			labels:     []label{{nodeCPU, "device"}},
		},
		{
			name:       "negative name matcher could read anything",
			expr:       `{__name__!="up", job="api"}`,
			couldMatch: []string{anything},
			labels:     []label{{anything, jobLabel}},
		},
		{
			name:       "selector without a name reads everything",
			expr:       `{job="api"}`,
			couldMatch: []string{anything},
		},
		{
			name:       "variable in a metric name",
			expr:       `node_` + Placeholder + `_total`,
			notMetrics: []string{"node_" + Placeholder + "_total", "node_cpu_total"},
			couldMatch: []string{"node_cpu_total"},
			noMatch:    []string{"node_cpu_seconds"},
		},
		{
			name:       "variable in a name matcher",
			expr:       `{__name__=~"` + Placeholder + `"}`,
			couldMatch: []string{anything},
		},
		{
			name:    "variable in a grouping could be any label",
			expr:    `sum by (` + Placeholder + `) (a_total)`,
			metrics: []string{aTotal},
			labels:  []label{{aTotal, podLabel}},
		},
		{
			name:    "label_replace reads its source label",
			expr:    `label_replace(a_total, "dst", "$1", "pod", "(.*)")`,
			metrics: []string{aTotal},
			labels:  []label{{aTotal, podLabel}},
		},
		{
			name:    "label_join reads its source labels",
			expr:    `label_join(a_total, "dst", "-", "pod", "node")`,
			metrics: []string{aTotal},
			labels:  []label{{aTotal, podLabel}, {aTotal, "node"}},
		},
		{
			name:    "subquery",
			expr:    `max_over_time(rate(a_total[1m])[1h:5m])`,
			metrics: []string{aTotal},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u := New()
			if err := u.Add(tt.expr); err != nil {
				t.Fatalf("Add(%q): %v", tt.expr, err)
			}
			for _, m := range tt.metrics {
				if !u.MetricUsed(m) {
					t.Errorf("MetricUsed(%q) = false", m)
				}
			}
			for _, m := range tt.notMetrics {
				if u.MetricUsed(m) {
					t.Errorf("MetricUsed(%q) = true", m)
				}
			}
			for _, m := range tt.couldMatch {
				if !u.CouldMatch(m) {
					t.Errorf("CouldMatch(%q) = false", m)
				}
			}
			for _, m := range tt.noMatch {
				if u.CouldMatch(m) {
					t.Errorf("CouldMatch(%q) = true", m)
				}
			}
			for _, l := range tt.labels {
				if !u.LabelUsed(l.metric, l.name) {
					t.Errorf("LabelUsed(%q, %q) = false", l.metric, l.name)
				}
			}
			for _, l := range tt.unusedLabels {
				if u.LabelUsed(l.metric, l.name) {
					t.Errorf("LabelUsed(%q, %q) = true", l.metric, l.name)
				}
			}
		})
	}
}

func TestAddInvalid(t *testing.T) {
	u := New()
	if err := u.Add(`rate(`); err == nil {
		t.Error("want a parse error")
	}
}
