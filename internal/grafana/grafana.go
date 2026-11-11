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

// Package grafana reads the PromQL out of Grafana dashboards.
package grafana

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/pwgreco/kardigan/internal/usage"
)

const (
	searchPath    = "/api/search"
	dashboardPath = "/api/dashboards/uid"

	pageSize     = 1000
	maxWorkers   = 5
	maxErrorBody = 512

	// maxResponseBody bounds what one response can make the operator hold in
	// memory, and up to maxWorkers responses are held at once. A dashboard this
	// large stops the analysis: skipping it would make its metrics look unused.
	maxResponseBody = 16 << 20
	// maxSearchPages stops a Grafana that always returns a full page from keeping
	// the listing going forever.
	maxSearchPages = 100

	lokiType = "loki"
)

// Client reads dashboards from Grafana's HTTP API.
type Client struct {
	BaseURL string       // e.g. http://localhost:9000
	Token   string       // service account token; empty for anonymous access
	HTTP    *http.Client // callers set a Timeout; nil means http.DefaultClient
}

// Expressions returns the PromQL of every panel and template variable in every
// dashboard the token can see, with template variables substituted. It fails if
// any dashboard can't be read.
func (c *Client) Expressions(ctx context.Context) ([]string, error) {
	uids, err := c.dashboardUIDs(ctx)
	if err != nil {
		return nil, err
	}

	var (
		mu    sync.Mutex
		exprs []string
	)
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(maxWorkers)
	for _, uid := range uids {
		group.Go(func() error {
			var out struct {
				Dashboard dashboard `json:"dashboard"`
			}
			if err := c.getJSON(ctx, dashboardPath+"/"+url.PathEscape(uid), nil, &out); err != nil {
				return fmt.Errorf("dashboard %s: %w", uid, err)
			}
			found := out.Dashboard.expressions()
			mu.Lock()
			defer mu.Unlock()
			exprs = append(exprs, found...)
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return exprs, nil
}

func (c *Client) dashboardUIDs(ctx context.Context) ([]string, error) {
	var uids []string
	for page := 1; page <= maxSearchPages; page++ {
		query := url.Values{}
		query.Set("type", "dash-db")
		query.Set("limit", strconv.Itoa(pageSize))
		query.Set("page", strconv.Itoa(page))

		var results []struct {
			UID string `json:"uid"`
		}
		if err := c.getJSON(ctx, searchPath, query, &results); err != nil {
			return nil, fmt.Errorf("listing dashboards: %w", err)
		}
		for _, r := range results {
			uids = append(uids, r.UID)
		}
		if len(results) < pageSize {
			return uids, nil
		}
	}
	return nil, fmt.Errorf("listing dashboards: more than %d pages of results", maxSearchPages)
}

func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	endpoint, err := url.JoinPath(c.BaseURL, path)
	if err != nil {
		return fmt.Errorf("building grafana URL: %w", err)
	}
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("building grafana request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("querying grafana: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The status is the error; an unreadable body only loses detail.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return fmt.Errorf("grafana returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return fmt.Errorf("reading grafana response: %w", err)
	}
	if len(body) > maxResponseBody {
		return fmt.Errorf("grafana response is larger than %d bytes", maxResponseBody)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding grafana response: %w", err)
	}
	return nil
}

type dashboard struct {
	Panels     []panel `json:"panels"`
	Templating struct {
		List []variable `json:"list"`
	} `json:"templating"`
}

type panel struct {
	Datasource datasource `json:"datasource"`
	Targets    []target   `json:"targets"`
	Panels     []panel    `json:"panels"` // collapsed rows hold their panels here
}

type target struct {
	Datasource datasource `json:"datasource"`
	Expr       string     `json:"expr"`
}

type variable struct {
	Type       string          `json:"type"`
	Datasource datasource      `json:"datasource"`
	Query      json.RawMessage `json:"query"`
}

// datasource is an object in current dashboards and a bare name in old ones.
type datasource struct {
	Type string
}

// UnmarshalJSON keeps the type when there is one. A name alone says nothing about
// the type, so it's left empty.
func (d *datasource) UnmarshalJSON(data []byte) error {
	var obj struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &obj); err == nil {
		d.Type = obj.Type
	}
	return nil
}

// isLogQL reports whether expressions on this data source are LogQL, which has to
// be skipped: rate({app="x"}[5m]) also parses as PromQL, and would seem to read
// every metric. Other types are parsed even if unfamiliar, because several plugins
// speak PromQL, and skipping one would make its metrics look unused.
func (d datasource) isLogQL() bool {
	return d.Type == lokiType
}

func (d dashboard) expressions() []string {
	var exprs []string
	for _, p := range d.Panels {
		exprs = append(exprs, p.expressions()...)
	}
	for _, v := range d.Templating.List {
		if v.Type != "query" || v.Datasource.isLogQL() {
			continue
		}
		if expr := variableExpression(v.Query); expr != "" {
			exprs = append(exprs, substitute(expr))
		}
	}
	return exprs
}

func (p panel) expressions() []string {
	var exprs []string
	for _, t := range p.Targets {
		ds := t.Datasource
		if ds.Type == "" {
			ds = p.Datasource
		}
		if t.Expr == "" || ds.isLogQL() {
			continue
		}
		exprs = append(exprs, substitute(t.Expr))
	}
	for _, nested := range p.Panels {
		exprs = append(exprs, nested.expressions()...)
	}
	return exprs
}

var (
	labelValuesQuery = regexp.MustCompile(`^\s*label_values\(\s*(.+),\s*([a-zA-Z_][a-zA-Z0-9_.]*)\s*\)\s*$`)
	queryResultQuery = regexp.MustCompile(`^\s*query_result\(\s*(.+)\s*\)\s*$`)
)

// variableExpression turns a template variable's query into PromQL, so what it
// reads counts as used: dropping that metric would empty the dropdown. The query
// is a string in old dashboards and an object in current ones. Queries that don't
// name a metric, such as label_values(pod), return nothing.
func variableExpression(raw json.RawMessage) string {
	var query string
	if err := json.Unmarshal(raw, &query); err != nil {
		var obj struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return ""
		}
		query = obj.Query
	}

	if m := labelValuesQuery.FindStringSubmatch(query); m != nil {
		return fmt.Sprintf("group by (%s) (%s)", m[2], m[1])
	}
	if m := queryResultQuery.FindStringSubmatch(query); m != nil {
		return m[1]
	}
	return ""
}

var (
	builtinVar = regexp.MustCompile(`\$\{?__\w+(:\w+)?\}?`)                           // $__rate_interval, ${__interval}
	durationAt = regexp.MustCompile(`(\[\s*|\boffset\s+)(\$\{[^}]+\}|\$\w+)(\s*\])?`) // [$window], offset $shift
	userVar    = regexp.MustCompile(`\$\{[^}]+\}|\$\w+|\[\[\w+\]\]`)                  // ${ns}, $ns, [[ns]]
)

// substitute replaces template variables, which aren't valid PromQL. It isn't
// perfect: a variable used as a whole selector still fails to parse, so callers
// count parse failures rather than treating them as fatal.
func substitute(expr string) string {
	expr = builtinVar.ReplaceAllString(expr, "5m")
	expr = durationAt.ReplaceAllString(expr, "${1}5m${3}")
	return userVar.ReplaceAllString(expr, usage.Placeholder)
}
