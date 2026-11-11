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
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/common/model"
	"sigs.k8s.io/yaml"

	kardiganv1alpha1 "github.com/pwgreco/kardigan/api/v1alpha1"
)

// relabelRule is one entry of a ServiceMonitor's metricRelabelings.
type relabelRule struct {
	SourceLabels []string `json:"sourceLabels"`
	Regex        string   `json:"regex"`
	TargetLabel  string   `json:"targetLabel,omitempty"`
	Replacement  *string  `json:"replacement,omitempty"`
	Action       string   `json:"action"`
}

// Snippet returns metricRelabelings YAML for the recommendations that relabelling
// can carry out, or an empty string if there are none.
//
// Labels are removed by setting them to an empty value on the metrics concerned,
// not with labeldrop. labeldrop has no way to name a metric, so it would remove
// the label from everything in the scrape, including metrics where nothing
// checked that the series stay distinct.
func Snippet(name string, generatedAt time.Time, result Result) (string, error) {
	var metrics []string
	labels := map[string][]string{}
	for _, rec := range result.Recommendations {
		switch rec.Action {
		case kardiganv1alpha1.ActionDropMetric:
			metrics = append(metrics, rec.Metric)
		case kardiganv1alpha1.ActionDropLabel:
			labels[rec.Label] = append(labels[rec.Label], rec.Metric)
		}
	}
	if len(metrics) == 0 && len(labels) == 0 {
		return "", nil
	}

	var rules []relabelRule
	if len(metrics) > 0 {
		rules = append(rules, relabelRule{
			SourceLabels: []string{model.MetricNameLabel},
			Regex:        namesRegex(metrics),
			Action:       "drop",
		})
	}
	empty := ""
	for _, label := range slices.Sorted(maps.Keys(labels)) {
		rules = append(rules, relabelRule{
			SourceLabels: []string{model.MetricNameLabel},
			Regex:        namesRegex(labels[label]),
			TargetLabel:  label,
			Replacement:  &empty,
			Action:       "replace",
		})
	}

	body, err := yaml.Marshal(map[string][]relabelRule{"metricRelabelings": rules})
	if err != nil {
		return "", fmt.Errorf("marshalling relabel rules: %w", err)
	}
	header := fmt.Sprintf("# Kardigan recommendations for %s, %s. Saves ~%d series.\n",
		name, generatedAt.Format(time.DateOnly), result.PotentialSavings())
	return header + string(body), nil
}

func namesRegex(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, regexp.QuoteMeta(name))
	}
	slices.Sort(quoted)
	return strings.Join(slices.Compact(quoted), "|")
}
