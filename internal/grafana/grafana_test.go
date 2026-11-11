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

package grafana

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/pwgreco/kardigan/internal/usage"
)

const rateExpr = `rate(a_total[5m])`

func TestSubstitute(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no variables", rateExpr, rateExpr},
		{"builtin range", `rate(a_total[$__rate_interval])`, rateExpr},
		{"builtin with braces", `rate(a_total[${__interval}])`, rateExpr},
		{"user range", `rate(a_total[$window])`, rateExpr},
		{"user range with braces", `rate(a_total[${window}])`, rateExpr},
		{"offset", `a_total offset $shift`, `a_total offset 5m`},
		{"label value", `a_total{ns="$ns"}`, `a_total{ns="` + usage.Placeholder + `"}`},
		{"label value with format", `a_total{ns=~"${ns:regex}"}`, `a_total{ns=~"` + usage.Placeholder + `"}`},
		{"old bracket form", `a_total{ns="[[ns]]"}`, `a_total{ns="` + usage.Placeholder + `"}`},
		{"grouping", `sum by ($group) (a_total)`, `sum by (` + usage.Placeholder + `) (a_total)`},
		{"metric name", `node_${res}_total`, `node_` + usage.Placeholder + `_total`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := substitute(tt.in)
			if got != tt.want {
				t.Errorf("substitute(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if err := usage.New().Add(got); err != nil {
				t.Errorf("result %q doesn't parse: %v", got, err)
			}
		})
	}
}

func TestVariableExpression(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"old string form", `"label_values(up{job=\"a\"}, pod)"`, `group by (pod) (up{job="a"})`},
		{"object form", `{"query":"label_values(a_total, pod)","refId":"x"}`, `group by (pod) (a_total)`},
		{"query_result", `"query_result(count(a_total))"`, `count(a_total)`},
		{"label name only reads no metric", `"label_values(pod)"`, ""},
		{"not promql", `"metrics(node_.*)"`, ""},
		{"neither form", `42`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := variableExpression([]byte(tt.raw)); got != tt.want {
				t.Errorf("variableExpression(%s) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

const dashboardJSON = `{"dashboard": {
  "panels": [
    {"type": "timeseries", "datasource": {"type": "prometheus"},
     "targets": [{"expr": "rate(direct_total[$__rate_interval])"}, {"refId": "no expr"}]},
    {"type": "row", "panels": [
      {"targets": [{"datasource": {"type": "prometheus"}, "expr": "nested_total"}]}
    ]},
    {"datasource": {"type": "loki"}, "targets": [{"expr": "rate({app=\"x\"}[5m])"}]},
    {"datasource": {"type": "datasource", "uid": "-- Mixed --"}, "targets": [
      {"datasource": {"type": "loki"}, "expr": "rate({app=\"y\"}[5m])"},
      {"datasource": {"type": "prometheus"}, "expr": "mixed_total"}
    ]},
    {"datasource": "old-style-name", "targets": [{"expr": "named_datasource_total"}]},
    {"datasource": {"type": "some-promql-plugin"}, "targets": [{"expr": "plugin_total"}]}
  ],
  "templating": {"list": [
    {"type": "query", "datasource": {"type": "prometheus"}, "query": "label_values(variable_total, pod)"},
    {"type": "query", "datasource": {"type": "loki"}, "query": "label_values(loki_thing, pod)"},
    {"type": "custom", "query": "a,b,c"}
  ]}
}}`

func TestExpressions(t *testing.T) {
	const token = "secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
		}
		switch r.URL.Path {
		case searchPath:
			if got := r.URL.Query().Get("type"); got != "dash-db" {
				t.Errorf("type = %q", got)
			}
			_, _ = w.Write([]byte(`[{"uid":"one"}]`))
		case "/api/dashboards/uid/one":
			_, _ = w.Write([]byte(dashboardJSON))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Token: token}
	got, err := c.Expressions(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"rate(direct_total[5m])",
		"nested_total",
		"mixed_total",
		"named_datasource_total",
		"plugin_total",
		"group by (pod) (variable_total)",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Expressions() = %q\nwant %q", got, want)
	}
}

func TestExpressionsPaginates(t *testing.T) {
	pages := []int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != searchPath {
			_, _ = w.Write([]byte(`{"dashboard":{"panels":[{"targets":[{"expr":"a_total"}]}]}}`))
			return
		}
		var page int
		_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
		pages = append(pages, page)
		count := pageSize
		if page == 2 {
			count = 1
		}
		uids := make([]string, count)
		for i := range uids {
			uids[i] = fmt.Sprintf(`{"uid":"p%d-%d"}`, page, i)
		}
		_, _ = fmt.Fprintf(w, "[%s]", strings.Join(uids, ","))
	}))
	defer srv.Close()

	got, err := (&Client{BaseURL: srv.URL}).Expressions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != pageSize+1 {
		t.Errorf("got %d expressions, want %d", len(got), pageSize+1)
	}
	if !slices.Equal(pages, []int{1, 2}) {
		t.Errorf("requested pages %v, want [1 2]", pages)
	}
}

// One unreadable dashboard has to fail the whole run: its metrics would otherwise
// look unused, and be recommended for dropping.
func TestExpressionsFailsOnAnyDashboard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case searchPath:
			_, _ = w.Write([]byte(`[{"uid":"good"},{"uid":"bad"}]`))
		case "/api/dashboards/uid/good":
			_, _ = w.Write([]byte(`{"dashboard":{}}`))
		default:
			http.Error(w, "nope", http.StatusForbidden)
		}
	}))
	defer srv.Close()

	_, err := (&Client{BaseURL: srv.URL}).Expressions(context.Background())
	if err == nil || !strings.Contains(err.Error(), "dashboard bad") || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v", err)
	}
}

func TestExpressionsSearchFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := (&Client{BaseURL: srv.URL}).Expressions(context.Background()); err == nil {
		t.Error("want an error")
	}
}

func TestOversizedResponseFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat(" ", maxResponseBody+1)))
	}))
	defer srv.Close()

	_, err := (&Client{BaseURL: srv.URL}).Expressions(context.Background())
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("err = %v, want one about the response size", err)
	}
}

func TestEndlessSearchResultsFail(t *testing.T) {
	var fullPage strings.Builder
	fullPage.WriteString("[")
	for i := range pageSize {
		if i > 0 {
			fullPage.WriteString(",")
		}
		fmt.Fprintf(&fullPage, `{"uid":"d%d"}`, i)
	}
	fullPage.WriteString("]")

	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(fullPage.String()))
	}))
	defer srv.Close()

	if _, err := (&Client{BaseURL: srv.URL}).Expressions(context.Background()); err == nil {
		t.Error("want an error")
	}
	if requests != maxSearchPages {
		t.Errorf("requests = %d, want %d", requests, maxSearchPages)
	}
}
