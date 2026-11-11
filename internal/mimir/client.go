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

// Package mimir is a minimal client for Grafana Mimir's cardinality API.
package mimir

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	labelValuesPath = "/prometheus/api/v1/cardinality/label_values"
	maxErrorBody    = 512

	// maxResponseBody bounds what one response can make the operator hold in
	// memory. Mimir caps cardinality results at 500 entries, so only an unusually
	// large set of rules gets near it, and failing is safer than running out.
	maxResponseBody = 32 << 20
)

// SeriesCounter counts active series matching label=value. The reconciler
// depends on this interface so tests can substitute a fake.
type SeriesCounter interface {
	CountSeries(ctx context.Context, label, value string) (int64, error)
}

var _ SeriesCounter = (*Client)(nil)

// Client counts active series through Mimir's cardinality API.
type Client struct {
	BaseURL string       // e.g. http://localhost:9009 (no trailing slash needed)
	Tenant  string       // sent as X-Scope-OrgID when non-empty
	HTTP    *http.Client // callers set a Timeout; nil means http.DefaultClient
}

// StatusError is returned when Mimir answers with a non-200 status.
type StatusError struct {
	Code int
	Body string // first 512 bytes of the response body, for debugging
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("mimir returned HTTP %d: %s", e.Code, e.Body)
}

// CountSeries returns the number of active series whose label equals value.
//
// It sends:
//
//	GET {BaseURL}/prometheus/api/v1/cardinality/label_values
//	    ?label_names[]={label}
//	    &selector={label="value"}
//	    &count_method=active
//
// and reads series_count from the response's entry for label. An empty labels
// list means nothing matched, so the count is zero.
//
// It does not use series_count_total: Mimir reports the whole tenant's total
// there, whatever the selector.
func (c *Client) CountSeries(ctx context.Context, label, value string) (int64, error) {
	query := url.Values{}
	query.Set("label_names[]", label)
	query.Set("selector", fmt.Sprintf("{%s=%s}", label, strconv.Quote(value)))
	query.Set("count_method", "active")

	var out struct {
		Labels []struct {
			LabelName   string `json:"label_name"`
			SeriesCount int64  `json:"series_count"`
		} `json:"labels"`
	}
	if err := c.getJSON(ctx, labelValuesPath, query, &out); err != nil {
		return 0, err
	}
	// Nil means the field was absent, which is not the same as Mimir's empty
	// list: treating it as zero would report the target as within budget.
	if out.Labels == nil {
		return 0, errors.New("mimir response has no labels field")
	}
	for _, l := range out.Labels {
		if l.LabelName == label {
			return l.SeriesCount, nil
		}
	}
	return 0, nil
}

// getJSON sends a GET request to path and decodes the JSON response into out.
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	body, err := c.get(ctx, path, query)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding mimir response: %w", err)
	}
	return nil
}

// get sends a GET request to path and returns the body of a 200 response. Any
// other status is a *StatusError.
func (c *Client) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	endpoint, err := url.JoinPath(c.BaseURL, path)
	if err != nil {
		return nil, fmt.Errorf("building mimir URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("building mimir request: %w", err)
	}
	if c.Tenant != "" {
		req.Header.Set("X-Scope-OrgID", c.Tenant)
	}

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying mimir: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The status is the error; an unreadable body only loses detail.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, &StatusError{Code: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody+1))
	if err != nil {
		return nil, fmt.Errorf("reading mimir response: %w", err)
	}
	if len(body) > maxResponseBody {
		return nil, fmt.Errorf("mimir response is larger than %d bytes", maxResponseBody)
	}
	return body, nil
}
