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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Actions a Recommendation can suggest.
const (
	// ActionDropMetric means nothing reads the metric, so it can be dropped.
	ActionDropMetric = "DropMetric"
	// ActionDropLabel means the label can be removed from the metric without
	// merging any series. It saves no active series.
	ActionDropLabel = "DropLabel"
	// ActionFixLabel means the label drives the metric's series count, but
	// removing it would merge series, so it has to change at the source.
	ActionFixLabel = "FixLabel"
	// ActionTrimHistogram means the histogram has more buckets than it needs.
	ActionTrimHistogram = "TrimHistogram"
	// ActionKeep means the metric is in use.
	ActionKeep = "Keep"
)

// Confidence levels of a Recommendation.
const (
	ConfidenceLow    = "Low"
	ConfidenceMedium = "Medium"
)

// CardinalityReportSpec defines the desired state of CardinalityReport.
type CardinalityReportSpec struct {
	// BudgetRef names the CardinalityBudget in the same namespace.
	// +kubebuilder:validation:MinLength=1
	// +required
	BudgetRef string `json:"budgetRef"`
}

// Recommendation is one suggested change, or a note that a metric should stay.
type Recommendation struct {
	// +kubebuilder:validation:Enum=DropMetric;DropLabel;FixLabel;TrimHistogram;Keep
	// +required
	Action string `json:"action"`

	// +required
	Metric string `json:"metric"`

	// Label is set for DropLabel and FixLabel.
	// +optional
	Label string `json:"label,omitempty"`

	// SeriesSaved is the active series this change would remove. Savings for
	// labels and histograms on the same metric overlap, so don't add them up.
	// +required
	SeriesSaved int64 `json:"seriesSaved"`

	// Confidence is empty for Keep.
	// +kubebuilder:validation:Enum=Low;Medium
	// +optional
	Confidence string `json:"confidence,omitempty"`

	// +required
	Reason string `json:"reason"`
}

// CardinalityReportStatus defines the observed state of CardinalityReport.
type CardinalityReportStatus struct {
	// GeneratedAt is when Recommendations were last worked out. After a failed
	// run they stay as they were, and the Ready condition says what went wrong.
	// +optional
	GeneratedAt *metav1.Time `json:"generatedAt,omitempty"`

	// TotalSeries is the target's active series.
	// +optional
	TotalSeries int64 `json:"totalSeries,omitempty"`

	// PotentialSavings is the active series RelabelSnippet would remove.
	// +optional
	PotentialSavings int64 `json:"potentialSavings,omitempty"`

	// Recommendations is capped at 25, largest savings first.
	// +listType=atomic
	// +optional
	Recommendations []Recommendation `json:"recommendations,omitempty"`

	// RelabelSnippet is YAML to paste into a ServiceMonitor or PodMonitor
	// endpoint. Kardigan never applies it.
	// +optional
	RelabelSnippet string `json:"relabelSnippet,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=crpt
// +kubebuilder:printcolumn:name="Budget",type=string,JSONPath=`.spec.budgetRef`
// +kubebuilder:printcolumn:name="Series",type=integer,JSONPath=`.status.totalSeries`
// +kubebuilder:printcolumn:name="Savings",type=integer,JSONPath=`.status.potentialSavings`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Generated",type=date,JSONPath=`.status.generatedAt`

// CardinalityReport is the Schema for the cardinalityreports API. Kardigan
// creates one for each CardinalityBudget.
type CardinalityReport struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of CardinalityReport
	// +required
	Spec CardinalityReportSpec `json:"spec"`

	// status defines the observed state of CardinalityReport
	// +optional
	Status CardinalityReportStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CardinalityReportList contains a list of CardinalityReport
type CardinalityReportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CardinalityReport `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &CardinalityReport{}, &CardinalityReportList{})
		return nil
	})
}
