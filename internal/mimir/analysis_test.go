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

package mimir

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

const testTenant = "demo"

// respond returns a client whose server answers every request with status and body.
func respond(t *testing.T, status int, body string, check func(r *http.Request)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Scope-OrgID"); got != testTenant {
			t.Errorf("X-Scope-OrgID = %q", got)
		}
		if check != nil {
			check(r)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Tenant: testTenant}
}

func TestMetricSelector(t *testing.T) {
	got := MetricSelector("a_total", "job", `team"a`)
	want := `{__name__="a_total",job="team\"a"}`
	if got != want {
		t.Errorf("MetricSelector() = %s, want %s", got, want)
	}
}

func TestSeriesPerMetric(t *testing.T) {
	// Copied from Mimir 3.2.4. series_count_total covers the whole tenant.
	const body = `{"series_count_total": 36535, "labels": [{
		"label_name": "__name__", "label_values_count": 3, "series_count": 3300,
		"cardinality": [
			{"label_value": "small", "series_count": 100},
			{"label_value": "b_bucket", "series_count": 1600},
			{"label_value": "a_bucket", "series_count": 1600}
		]}]}`

	c := respond(t, http.StatusOK, body, func(r *http.Request) {
		if r.URL.Path != labelValuesPath {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if got := q.Get("label_names[]"); got != "__name__" {
			t.Errorf("label_names[] = %q", got)
		}
		if got := q.Get("selector"); got != `{job="team-c"}` {
			t.Errorf("selector = %q", got)
		}
		if got := q.Get("count_method"); got != "active" {
			t.Errorf("count_method = %q", got)
		}
	})

	metrics, total, err := c.SeriesPerMetric(context.Background(), "job", "team-c")
	if err != nil {
		t.Fatal(err)
	}
	if total != 3300 {
		t.Errorf("total = %d, want 3300, not the tenant's total", total)
	}
	want := []NameCount{{"a_bucket", 1600}, {"b_bucket", 1600}, {"small", 100}}
	if !slices.Equal(metrics, want) {
		t.Errorf("metrics = %v, want %v, largest first with ties by name", metrics, want)
	}
}

func TestSeriesPerMetricNoMatches(t *testing.T) {
	c := respond(t, http.StatusOK, `{"series_count_total": 36535, "labels": []}`, nil)
	metrics, total, err := c.SeriesPerMetric(context.Background(), "job", "nobody")
	if err != nil || total != 0 || len(metrics) != 0 {
		t.Errorf("metrics = %v, total = %d, err = %v", metrics, total, err)
	}
}

func TestSeriesPerMetricMissingLabels(t *testing.T) {
	c := respond(t, http.StatusOK, `{}`, nil)
	if _, _, err := c.SeriesPerMetric(context.Background(), "job", "x"); err == nil {
		t.Error("a response with no labels field should be an error, not zero series")
	}
}

func TestLabelValueCounts(t *testing.T) {
	const body = `{"label_values_count_total": 12, "label_names_count": 3, "cardinality": [
		{"label_name": "cluster", "label_values_count": 1},
		{"label_name": "series_id", "label_values_count": 10},
		{"label_name": "__name__", "label_values_count": 1}]}`

	c := respond(t, http.StatusOK, body, func(r *http.Request) {
		if r.URL.Path != "/prometheus/api/v1/cardinality/label_names" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("selector"); got != `{job="x"}` {
			t.Errorf("selector = %q", got)
		}
	})

	got, err := c.LabelValueCounts(context.Background(), `{job="x"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []NameCount{{"series_id", 10}, {"__name__", 1}, {"cluster", 1}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestQueryCount(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    int64
		wantErr bool
	}{
		{name: "count", body: `{"status":"success","data":{"resultType":"vector",
			"result":[{"metric":{},"value":[1793000000.1,"150"]}]}}`, want: 150},
		{name: "no series", body: `{"status":"success","data":{"resultType":"vector","result":[]}}`, want: 0},
		{name: "several results", body: `{"status":"success","data":{"resultType":"vector",
			"result":[{"value":[1,"1"]},{"value":[1,"2"]}]}}`, wantErr: true},
		{name: "scalar", body: `{"status":"success","data":{"resultType":"scalar","result":[1,"1"]}}`, wantErr: true},
		{name: "value not a string", body: `{"data":{"resultType":"vector","result":[{"value":[1,2]}]}}`, wantErr: true},
		{name: "value not a number", body: `{"data":{"resultType":"vector","result":[{"value":[1,"x"]}]}}`, wantErr: true},
		{name: "short sample", body: `{"data":{"resultType":"vector","result":[{"value":[1]}]}}`, wantErr: true},
		{name: "empty body", body: `{}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := respond(t, http.StatusOK, tt.body, func(r *http.Request) {
				if r.URL.Path != "/prometheus/api/v1/query" {
					t.Errorf("path = %s", r.URL.Path)
				}
				if got := r.URL.Query().Get("query"); got != "count(x)" {
					t.Errorf("query = %q", got)
				}
			})
			got, err := c.QueryCount(context.Background(), "count(x)")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestRateLimitIsAnError(t *testing.T) {
	c := respond(t, http.StatusTooManyRequests, "too many outstanding requests", nil)
	_, err := c.QueryCount(context.Background(), "count(x)")
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Code != http.StatusTooManyRequests {
		t.Errorf("err = %v, want a *StatusError with 429", err)
	}
}

func TestRuleExpressions(t *testing.T) {
	const body = `
team-a:
  - name: recording
    rules:
      - record: job:a:rate5m
        expr: sum by (job) (rate(a_total[5m]))
team-b:
  - name: alerts
    rules:
      - alert: High
        expr: b_total > 1
        for: 5m
`
	c := respond(t, http.StatusOK, body, func(r *http.Request) {
		if r.URL.Path != "/prometheus/config/v1/rules" {
			t.Errorf("path = %s", r.URL.Path)
		}
	})
	got, err := c.RuleExpressions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{"b_total > 1", "sum by (job) (rate(a_total[5m]))"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRuleExpressionsNoRules(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"empty document", http.StatusOK, "{}"},
		{"not found", http.StatusNotFound, "no rule groups found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := respond(t, tt.status, tt.body, nil).RuleExpressions(context.Background())
			if err != nil || len(got) != 0 {
				t.Errorf("got %q, err = %v", got, err)
			}
		})
	}
}

func TestRuleExpressionsFailure(t *testing.T) {
	c := respond(t, http.StatusInternalServerError, "boom", nil)
	if _, err := c.RuleExpressions(context.Background()); err == nil {
		t.Error("a ruler failure shouldn't look like having no rules")
	}
}
