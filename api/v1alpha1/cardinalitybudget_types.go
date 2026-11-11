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

// BudgetTarget selects the series a budget counts: every active series whose
// Label equals Value.
type BudgetTarget struct {
	// Label is the series label to match on.
	// +kubebuilder:validation:Enum=job;namespace
	// +required
	Label string `json:"label"`

	// Value is the label value that identifies the team, e.g. "team-a".
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +required
	Value string `json:"value"`
}

// CardinalityBudgetSpec defines the desired state of CardinalityBudget.
type CardinalityBudgetSpec struct {
	// +required
	Target BudgetTarget `json:"target"`

	// MaxSeries is the number of active series the target may use.
	// +kubebuilder:validation:Minimum=1
	// +required
	MaxSeries int64 `json:"maxSeries"`

	// WarnAtPercent is the usage percentage that triggers WarningThreshold.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=80
	// +optional
	WarnAtPercent int32 `json:"warnAtPercent,omitempty"`

	// Interval is how often to re-check Mimir. The floor protects Mimir: each
	// check is one query per budget.
	// +kubebuilder:default="5m"
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('30s')",message="interval must be at least 30s"
	// +optional
	Interval metav1.Duration `json:"interval,omitzero"`
}

// CardinalityBudgetStatus defines the observed state of CardinalityBudget.
type CardinalityBudgetStatus struct {
	// +optional
	CurrentSeries int64 `json:"currentSeries,omitempty"`
	// +optional
	UsagePercent int32 `json:"usagePercent,omitempty"`
	// +optional
	LastChecked *metav1.Time `json:"lastChecked,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=cb
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.target.value`
// +kubebuilder:printcolumn:name="Series",type=integer,JSONPath=`.status.currentSeries`
// +kubebuilder:printcolumn:name="Max",type=integer,JSONPath=`.spec.maxSeries`
// +kubebuilder:printcolumn:name="Usage%",type=integer,JSONPath=`.status.usagePercent`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.conditions[?(@.type=="WithinBudget")].reason`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// CardinalityBudget is the Schema for the cardinalitybudgets API
type CardinalityBudget struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of CardinalityBudget
	// +required
	Spec CardinalityBudgetSpec `json:"spec"`

	// status defines the observed state of CardinalityBudget
	// +optional
	Status CardinalityBudgetStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CardinalityBudgetList contains a list of CardinalityBudget
type CardinalityBudgetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []CardinalityBudget `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &CardinalityBudget{}, &CardinalityBudgetList{})
		return nil
	})
}
