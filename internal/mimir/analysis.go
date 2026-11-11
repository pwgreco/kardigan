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
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"sigs.k8s.io/yaml"
)

const (
	labelNamesPath = "/prometheus/api/v1/cardinality/label_names"
	queryPath      = "/prometheus/api/v1/query"
	rulesPath      = "/prometheus/config/v1/rules"

	metricNameLabel = "__name__"

	// maxCardinalityLimit is the most results Mimir's cardinality API returns.
	maxCardinalityLimit = "500"
)

// NameCount pairs a metric or label name with a count.
type NameCount struct {
	Name  string
	Count int64
}

// MetricSelector returns a PromQL selector for one metric within a target.
func MetricSelector(metric, label, value string) string {
	return fmt.Sprintf("{%s=%s,%s=%s}", metricNameLabel, strconv.Quote(metric), label, strconv.Quote(value))
}

// SeriesPerMetric returns the target's metrics, largest first, up to Mimir's
// limit of 500, and the target's total active series.
func (c *Client) SeriesPerMetric(ctx context.Context, label, value string) ([]NameCount, int64, error) {
	query := url.Values{}
	query.Set("label_names[]", metricNameLabel)
	query.Set("selector", fmt.Sprintf("{%s=%s}", label, strconv.Quote(value)))
	query.Set("count_method", "active")
	query.Set("limit", maxCardinalityLimit)

	var out struct {
		Labels []struct {
			SeriesCount int64 `json:"series_count"`
			Cardinality []struct {
				LabelValue  string `json:"label_value"`
				SeriesCount int64  `json:"series_count"`
			} `json:"cardinality"`
		} `json:"labels"`
	}
	if err := c.getJSON(ctx, labelValuesPath, query, &out); err != nil {
		return nil, 0, err
	}
	if out.Labels == nil {
		return nil, 0, errors.New("mimir response has no labels field")
	}

	var metrics []NameCount
	var total int64
	for _, l := range out.Labels {
		total += l.SeriesCount
		for _, m := range l.Cardinality {
			metrics = append(metrics, NameCount{Name: m.LabelValue, Count: m.SeriesCount})
		}
	}
	sortByCount(metrics)
	return metrics, total, nil
}

// LabelValueCounts returns the labels on the series matching selector, with the
// number of distinct values each has, most values first.
func (c *Client) LabelValueCounts(ctx context.Context, selector string) ([]NameCount, error) {
	query := url.Values{}
	query.Set("selector", selector)
	query.Set("limit", maxCardinalityLimit)

	var out struct {
		Cardinality []struct {
			LabelName        string `json:"label_name"`
			LabelValuesCount int64  `json:"label_values_count"`
		} `json:"cardinality"`
	}
	if err := c.getJSON(ctx, labelNamesPath, query, &out); err != nil {
		return nil, err
	}

	labels := make([]NameCount, 0, len(out.Cardinality))
	for _, l := range out.Cardinality {
		labels = append(labels, NameCount{Name: l.LabelName, Count: l.LabelValuesCount})
	}
	sortByCount(labels)
	return labels, nil
}

// QueryCount runs an instant query expected to return a single number, such as
// a count(). An empty result, which is how count() reports no series, is zero.
func (c *Client) QueryCount(ctx context.Context, promql string) (int64, error) {
	query := url.Values{}
	query.Set("query", promql)

	var out struct {
		Data struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				// Value is [timestamp, "value"], where the two have different types.
				Value []any `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, queryPath, query, &out); err != nil {
		return 0, err
	}
	if out.Data.ResultType != "vector" {
		return 0, fmt.Errorf("mimir returned result type %q, want vector", out.Data.ResultType)
	}
	switch len(out.Data.Result) {
	case 0:
		return 0, nil
	case 1:
	default:
		return 0, fmt.Errorf("mimir returned %d results, want one", len(out.Data.Result))
	}
	value := out.Data.Result[0].Value
	if len(value) != 2 {
		return 0, fmt.Errorf("mimir returned a malformed sample: %v", value)
	}
	text, ok := value[1].(string)
	if !ok {
		return 0, fmt.Errorf("mimir returned a malformed sample value: %v", value[1])
	}
	n, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, fmt.Errorf("parsing mimir sample value: %w", err)
	}
	return int64(n), nil
}

// RuleExpressions returns the PromQL of every rule stored in Mimir's ruler. Some
// Mimir versions answer 404 when a tenant has no rules, so that counts as none.
func (c *Client) RuleExpressions(ctx context.Context) ([]string, error) {
	body, err := c.get(ctx, rulesPath, url.Values{})
	if err != nil {
		var statusErr *StatusError
		if errors.As(err, &statusErr) && statusErr.Code == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}

	// The response is YAML: rule namespace -> list of rule groups.
	var namespaces map[string][]struct {
		Rules []struct {
			Expr string `json:"expr"`
		} `json:"rules"`
	}
	if err := yaml.Unmarshal(body, &namespaces); err != nil {
		return nil, fmt.Errorf("decoding mimir rules: %w", err)
	}

	var exprs []string
	for _, groups := range namespaces {
		for _, group := range groups {
			for _, rule := range group.Rules {
				exprs = append(exprs, rule.Expr)
			}
		}
	}
	return exprs, nil
}

// sortByCount sorts largest first, breaking ties by name so output is deterministic.
func sortByCount(items []NameCount) {
	slices.SortFunc(items, func(a, b NameCount) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Name, b.Name))
	})
}
