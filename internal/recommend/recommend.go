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

// Package recommend decides which metrics and labels a team could drop.
package recommend

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/prometheus/common/model"

	kardiganv1alpha1 "github.com/pwgreco/kardigan/api/v1alpha1"
	"github.com/pwgreco/kardigan/internal/mimir"
	"github.com/pwgreco/kardigan/internal/usage"
)

const (
	// DashboardSource is the name of the usage source for Grafana dashboards.
	DashboardSource = "grafana"

	maxRecommendations = 25
	// maxKeep reserves room for Keep entries, which save nothing and would
	// otherwise always be cut off. Teams need them to see why a metric stayed.
	maxKeep = 5

	// maxLabelChecks limits the used metrics whose labels are examined, because
	// each one costs several queries.
	maxLabelChecks = 10
	// minLabelValues is how many distinct values make a label worth examining.
	minLabelValues = 10
	// maxBuckets is roughly how many histogram buckets are useful.
	maxBuckets = 15

	bucketLabel = "le"
)

// protectedLabels are never suggested for removal. job and instance identify the
// scrape target, and le and quantile carry a histogram's or summary's meaning.
var protectedLabels = map[string]bool{
	model.MetricNameLabel: true,
	"job":                 true,
	"instance":            true,
	bucketLabel:           true,
	"quantile":            true,
}

// plainLabelName matches label names that can go into PromQL unquoted. Names come
// from Mimir, and queries are built from them, so any other name is left alone.
var plainLabelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// generatedMetrics are written by Prometheus itself after metric relabelling runs,
// so metricRelabelings can't drop them.
var generatedMetrics = map[string]bool{
	"up":                                    true,
	"scrape_duration_seconds":               true,
	"scrape_samples_scraped":                true,
	"scrape_samples_post_metric_relabeling": true,
	"scrape_series_added":                   true,
}

// Source is what the analysis needs from Mimir.
type Source interface {
	SeriesPerMetric(ctx context.Context, label, value string) ([]mimir.NameCount, int64, error)
	LabelValueCounts(ctx context.Context, selector string) ([]mimir.NameCount, error)
	QueryCount(ctx context.Context, promql string) (int64, error)
}

// Result is the outcome of analysing one target.
type Result struct {
	TotalSeries     int64
	Recommendations []kardiganv1alpha1.Recommendation
}

// PotentialSavings returns the series RelabelSnippet would remove. Only dropped
// metrics count: savings on labels and histograms overlap, and need changes at
// the source that the snippet can't make.
func (r Result) PotentialSavings() int64 {
	var total int64
	for _, rec := range r.Recommendations {
		if rec.Action == kardiganv1alpha1.ActionDropMetric {
			total += rec.SeriesSaved
		}
	}
	return total
}

// Analyze compares what the target sends to Mimir with what the snapshot reads.
func Analyze(
	ctx context.Context, src Source, label, value string, snapshot *usage.Snapshot,
) (Result, error) {
	metrics, total, err := src.SeriesPerMetric(ctx, label, value)
	if err != nil {
		return Result{}, fmt.Errorf("listing metrics: %w", err)
	}

	var recs []kardiganv1alpha1.Recommendation
	checked := 0
	for _, metric := range metrics {
		switch {
		case generatedMetrics[metric.Name]:
		case !snapshot.Usage.MetricUsed(metric.Name):
			recs = append(recs, dropMetric(metric, snapshot))
		default:
			recs = append(recs, kardiganv1alpha1.Recommendation{
				Action: kardiganv1alpha1.ActionKeep,
				Metric: metric.Name,
				Reason: "Used in a dashboard or rule.",
			})
			if checked == maxLabelChecks {
				continue
			}
			checked++
			found, err := checkLabels(ctx, src, label, value, metric, snapshot.Usage)
			if err != nil {
				return Result{}, fmt.Errorf("checking labels of %s: %w", metric.Name, err)
			}
			recs = append(recs, found...)
		}
	}
	return Result{TotalSeries: total, Recommendations: limit(recs)}, nil
}

func dropMetric(metric mimir.NameCount, snapshot *usage.Snapshot) kardiganv1alpha1.Recommendation {
	confidence := kardiganv1alpha1.ConfidenceMedium
	notes := []string{}
	if snapshot.Usage.CouldMatch(metric.Name) {
		confidence = kardiganv1alpha1.ConfidenceLow
		notes = append(notes, "A selector with a regex or template variable could read it.")
	}
	if snapshot.ParseFailures > 0 {
		confidence = kardiganv1alpha1.ConfidenceLow
		notes = append(notes, fmt.Sprintf("%d expressions couldn't be parsed.", snapshot.ParseFailures))
	}
	if _, ok := snapshot.Expressions[DashboardSource]; !ok {
		confidence = kardiganv1alpha1.ConfidenceLow
		notes = append(notes, "Dashboards weren't checked.")
	}

	reason := fmt.Sprintf("Not in any of %d expressions from %s. Ad hoc queries aren't visible.",
		snapshot.Total(), strings.Join(slices.Sorted(maps.Keys(snapshot.Expressions)), ", "))
	return kardiganv1alpha1.Recommendation{
		Action:      kardiganv1alpha1.ActionDropMetric,
		Metric:      metric.Name,
		SeriesSaved: metric.Count,
		Confidence:  confidence,
		Reason:      strings.Join(append([]string{reason}, notes...), " "),
	}
}

// checkLabels looks for a used metric's labels that nothing relies on.
func checkLabels(
	ctx context.Context, src Source, label, value string, metric mimir.NameCount, used *usage.Usage,
) ([]kardiganv1alpha1.Recommendation, error) {
	selector := mimir.MetricSelector(metric.Name, label, value)
	labels, err := src.LabelValueCounts(ctx, selector)
	if err != nil {
		return nil, err
	}

	var recs []kardiganv1alpha1.Recommendation
	var removed []string // labels already recommended for removal from this metric
	for _, l := range labels {
		if l.Name == bucketLabel && l.Count > maxBuckets && strings.HasSuffix(metric.Name, "_bucket") {
			recs = append(recs, trimHistogram(metric, l.Count))
		}
		if protectedLabels[l.Name] || l.Name == label || l.Count < minLabelValues ||
			!plainLabelName.MatchString(l.Name) || used.LabelUsed(metric.Name, l.Name) {
			continue
		}
		// Mimir's label names include series that have gone, whatever the count
		// method, so a label removed by relabelling would keep coming back. Grouping
		// by a label that's absent gives one group, which is below the threshold.
		values, err := src.QueryCount(ctx, fmt.Sprintf("count(count by (%s) (%s))", l.Name, selector))
		if err != nil {
			return nil, err
		}
		if values < minLabelValues {
			continue
		}
		l.Count = values

		rec, err := checkUniqueness(ctx, src, selector, metric.Name, l, removed)
		if err != nil {
			return nil, err
		}
		if rec != nil {
			recs = append(recs, *rec)
			if rec.Action == kardiganv1alpha1.ActionDropLabel {
				removed = append(removed, l.Name)
			}
		}
	}
	return recs, nil
}

// checkUniqueness asks whether series would still be distinct without the label.
// If they would, the label is redundant: removing it is safe, but because the
// series all survive, it saves none of them. If they wouldn't, removing it saves
// series but makes them collide: Prometheus keeps one series and loses the other
// samples, with no error or failed target. So it has to change at the source.
//
// The check includes the labels already recommended for removal, because labels
// can be redundant only with each other. If two labels copy each other, series
// stay distinct without either one, but not without both.
func checkUniqueness(
	ctx context.Context, src Source, selector, metric string, label mimir.NameCount, removed []string,
) (*kardiganv1alpha1.Recommendation, error) {
	before, err := src.QueryCount(ctx, fmt.Sprintf("count(%s)", selector))
	if err != nil {
		return nil, err
	}
	if before == 0 {
		// Mimir still counts a metric as active for a while after its samples stop,
		// but queries find nothing. Zero equalling zero says nothing about the label.
		return nil, nil
	}
	without := strings.Join(append(slices.Clone(removed), label.Name), ", ")
	after, err := src.QueryCount(ctx, fmt.Sprintf("count(count without (%s) (%s))", without, selector))
	if err != nil {
		return nil, err
	}

	switch {
	case after == before:
		return &kardiganv1alpha1.Recommendation{
			Action:     kardiganv1alpha1.ActionDropLabel,
			Metric:     metric,
			Label:      label.Name,
			Confidence: kardiganv1alpha1.ConfidenceMedium,
			Reason: fmt.Sprintf("Nothing uses this label and it has %d values, but other labels already "+
				"identify each series. Removing it merges nothing and saves no active series.", label.Count),
		}, nil
	case after < before:
		condition := "Without it"
		if len(removed) > 0 {
			condition = fmt.Sprintf("Without it and %s", strings.Join(removed, ", "))
		}
		return &kardiganv1alpha1.Recommendation{
			Action:      kardiganv1alpha1.ActionFixLabel,
			Metric:      metric,
			Label:       label.Name,
			SeriesSaved: before - after,
			Confidence:  kardiganv1alpha1.ConfidenceLow,
			Reason: fmt.Sprintf("Nothing uses this label and it has %d values. %s, %d series become %d, "+
				"which relabelling would make collide, so remove it in the instrumentation.",
				label.Count, condition, before, after),
		}, nil
	default:
		// Series changed between the two queries, so the comparison means nothing.
		return nil, nil
	}
}

func trimHistogram(metric mimir.NameCount, buckets int64) kardiganv1alpha1.Recommendation {
	return kardiganv1alpha1.Recommendation{
		Action:      kardiganv1alpha1.ActionTrimHistogram,
		Metric:      metric.Name,
		SeriesSaved: metric.Count - metric.Count*maxBuckets/buckets,
		Confidence:  kardiganv1alpha1.ConfidenceLow,
		Reason: fmt.Sprintf("Has %d buckets, and about %d are usually enough. "+
			"Use fewer at the source, or native histograms.", buckets, maxBuckets),
	}
}

// limit keeps the largest savings and some Keep entries, so the report stays
// readable and well below the size limit of a Kubernetes object.
func limit(recs []kardiganv1alpha1.Recommendation) []kardiganv1alpha1.Recommendation {
	var actions, keeps []kardiganv1alpha1.Recommendation
	for _, rec := range recs {
		if rec.Action == kardiganv1alpha1.ActionKeep {
			keeps = append(keeps, rec)
		} else {
			actions = append(actions, rec)
		}
	}
	slices.SortStableFunc(actions, func(a, b kardiganv1alpha1.Recommendation) int {
		return cmp.Or(
			cmp.Compare(b.SeriesSaved, a.SeriesSaved),
			cmp.Compare(a.Metric, b.Metric),
			cmp.Compare(a.Label, b.Label),
			cmp.Compare(a.Action, b.Action),
		)
	})

	// Keep entries arrive largest first, because metrics do. Unused Keep slots go
	// to actions, and unused action slots go to Keep entries.
	keepRoom := min(len(keeps), maxKeep)
	actions = actions[:min(len(actions), maxRecommendations-keepRoom)]
	keeps = keeps[:min(len(keeps), maxRecommendations-len(actions))]
	return slices.Concat(actions, keeps)
}
