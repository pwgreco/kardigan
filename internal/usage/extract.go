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

// Package usage works out which metrics and labels PromQL expressions read.
package usage

import (
	"regexp"
	"slices"
	"strings"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// Placeholder stands in for a template variable whose value isn't known. Callers
// that substitute variables use it, so that a name containing a variable is
// treated as a pattern and not as a metric called "kardigan_var".
const Placeholder = "kardigan_var"

// Options allow everything Prometheus can parse: rejecting an expression over a
// feature flag would hide what it reads.
var promql = parser.NewParser(parser.Options{
	EnableExperimentalFunctions:  true,
	ExperimentalDurationExpr:     true,
	EnableExtendedRangeSelectors: true,
	EnableBinopFillModifiers:     true,
})

// Usage records which metrics expressions read and which labels they rely on.
// Everything errs towards "in use", because a metric wrongly reported as unused
// could end in someone deleting data a dashboard needs.
type Usage struct {
	metrics map[string]bool            // metric names selected
	labels  map[string]map[string]bool // metric -> labels used in matchers
	grouped map[string]bool            // labels used for grouping or joins, on any metric
	broad   []*regexp.Regexp           // selectors whose metric names can't be listed
}

// New returns an empty Usage.
func New() *Usage {
	return &Usage{
		metrics: map[string]bool{},
		labels:  map[string]map[string]bool{},
		grouped: map[string]bool{},
	}
}

// Add parses expr and records what it reads. Callers substitute any template
// variables first.
func (u *Usage) Add(expr string) error {
	node, err := promql.ParseExpr(expr)
	if err != nil {
		return err
	}
	parser.Inspect(node, func(n parser.Node, _ []parser.Node) error {
		switch n := n.(type) {
		case *parser.VectorSelector:
			u.addSelector(n)
		case *parser.AggregateExpr:
			// "without" also counts: those labels must exist to be left out.
			u.addGrouped(n.Grouping...)
		case *parser.BinaryExpr:
			if n.VectorMatching != nil {
				u.addGrouped(n.VectorMatching.MatchingLabels...)
				u.addGrouped(n.VectorMatching.Include...)
			}
		case *parser.Call:
			u.addGrouped(labelArguments(n)...)
		}
		return nil
	})
	return nil
}

func (u *Usage) addSelector(sel *parser.VectorSelector) {
	name := sel.Name
	exact := name != ""
	for _, m := range sel.LabelMatchers {
		if m.Name != model.MetricNameLabel {
			continue
		}
		switch m.Type {
		case labels.MatchEqual:
			name, exact = m.Value, true
		case labels.MatchRegexp:
			u.addBroad(m.Value)
		case labels.MatchNotEqual, labels.MatchNotRegexp:
			u.addBroad(".*")
		}
	}

	if exact && strings.Contains(name, Placeholder) {
		u.addBroad(regexp.QuoteMeta(name))
		exact = false
	}
	if !exact && !slices.ContainsFunc(sel.LabelMatchers, isNameMatcher) {
		// A selector like {job="api"} reads every metric the job has.
		u.addBroad(".*")
	}

	if exact {
		u.metrics[name] = true
	}
	for _, m := range sel.LabelMatchers {
		switch {
		case m.Name == model.MetricNameLabel:
		case exact:
			if u.labels[name] == nil {
				u.labels[name] = map[string]bool{}
			}
			u.labels[name][m.Name] = true
		default:
			// The label can't be tied to a metric, so protect it everywhere.
			u.grouped[m.Name] = true
		}
	}
}

func (u *Usage) addBroad(pattern string) {
	pattern = strings.ReplaceAll(pattern, Placeholder, ".*")
	// PromQL regexes are anchored at both ends.
	re, err := regexp.Compile("^(?s:" + pattern + ")$")
	if err != nil {
		re = regexp.MustCompile(".*")
	}
	u.broad = append(u.broad, re)
}

func (u *Usage) addGrouped(names ...string) {
	for _, name := range names {
		u.grouped[name] = true
	}
}

func isNameMatcher(m *labels.Matcher) bool {
	return m.Name == model.MetricNameLabel
}

// labelArguments returns the labels a function reads through string arguments.
func labelArguments(call *parser.Call) []string {
	var args parser.Expressions
	switch call.Func.Name {
	case "label_replace": // label_replace(v, dst, replacement, src, regex)
		if len(call.Args) > 3 {
			args = call.Args[3:4]
		}
	case "label_join": // label_join(v, dst, separator, src...)
		if len(call.Args) > 3 {
			args = call.Args[3:]
		}
	case "sort_by_label", "sort_by_label_desc": // sort_by_label(v, label...)
		args = call.Args[1:]
	}

	var names []string
	for _, arg := range args {
		if s, ok := arg.(*parser.StringLiteral); ok {
			names = append(names, s.Val)
		}
	}
	return names
}

// MetricUsed reports whether an expression selects the metric by name.
func (u *Usage) MetricUsed(metric string) bool {
	return u.metrics[metric]
}

// CouldMatch reports whether a selector whose metric names can't be listed, such
// as {__name__=~"node_.*"}, could read the metric.
func (u *Usage) CouldMatch(metric string) bool {
	return slices.ContainsFunc(u.broad, func(re *regexp.Regexp) bool {
		return re.MatchString(metric)
	})
}

// LabelUsed reports whether an expression relies on the label on this metric.
// Grouping isn't tied to a metric, so a label grouped on anywhere counts, and a
// variable in a grouping could be any label.
func (u *Usage) LabelUsed(metric, label string) bool {
	return u.labels[metric][label] || u.grouped[label] || u.grouped[Placeholder]
}
