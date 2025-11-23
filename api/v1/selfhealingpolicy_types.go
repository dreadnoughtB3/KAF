/*
Copyright 2025.

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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TriggerRule defines a specific condition based on a Prometheus query
type TriggerRule struct {
	// Name of the rule for logging and status identification
	Name string `json:"name"`

	// Query is the PromQL query to execute. It should return a vector or scalar.
	// Example: rate(tetragon_events_total{event_type="execve"}[1m])
	Query string `json:"query"`

	// Operator for comparison
	// +kubebuilder:validation:Enum=">";"<";"==";">=";"<="
	Operator string `json:"operator"`

	// Threshold value to compare against the query result
	// We use string to avoid float precision issues and allow parsing in controller
	// Example: "50", "0.5"
	Threshold string `json:"threshold"`

	// For specifies how long the condition must be true before triggering
	// Example: "1m", "30s"
	For string `json:"for,omitempty"`
}

// RecoveryStep defines a single action in the recovery pipeline
type RecoveryStep struct {
	// Name of the step
	Name string `json:"name"`

	// Action type to perform
	// +kubebuilder:validation:Enum="DeletePod";"CordonNode";"DrainNode";"RollbackDeployment";"Command"
	Action string `json:"action"`

	// Params are optional parameters for the action
	// Example: {"force": "true", "gracePeriod": "0"}
	Params map[string]string `json:"params,omitempty"`
}

// SelfHealingPolicySpec defines the desired state of SelfHealingPolicy
type SelfHealingPolicySpec struct {
	// Target selects the pods to be monitored and healed
	Target metav1.LabelSelector `json:"target"`

	// CheckInterval determines how often to query Prometheus
	// Default: "15s"
	// +kubebuilder:default="15s"
	CheckInterval string `json:"checkInterval,omitempty"`

	// TriggerLogic determines how multiple triggers are combined
	// +kubebuilder:validation:Enum="OR";"AND"
	// +kubebuilder:default="OR"
	TriggerLogic string `json:"triggerLogic,omitempty"`

	// Triggers is a list of conditions that activate the healing pipeline
	Triggers []TriggerRule `json:"triggers"`

	// Pipeline is the ordered list of recovery actions
	Pipeline []RecoveryStep `json:"pipeline"`

	// CooldownPeriod is the time to wait after a healing pipeline execution before resuming monitoring
	// +kubebuilder:default="5m"
	CooldownPeriod string `json:"cooldownPeriod,omitempty"`
}

// SelfHealingPolicyStatus defines the observed state of SelfHealingPolicy
type SelfHealingPolicyStatus struct {
	// LastExecutionTime is when the pipeline last ran
	LastExecutionTime *metav1.Time `json:"lastExecutionTime,omitempty"`

	// Phase describes the current state of the policy
	// +kubebuilder:validation:Enum="Healthy";"Detecting";"Healing";"Cooldown"
	Phase string `json:"phase,omitempty"`

	// CurrentPipelineStep indicates which step is currently running (if Phase is Healing)
	CurrentPipelineStep string `json:"currentPipelineStep,omitempty"`

	// TriggeredRule records which rule caused the healing
	TriggeredRule string `json:"triggeredRule,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status

// SelfHealingPolicy is the Schema for the selfhealingpolicies API
type SelfHealingPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SelfHealingPolicySpec   `json:"spec,omitempty"`
	Status SelfHealingPolicyStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// SelfHealingPolicyList contains a list of SelfHealingPolicy
type SelfHealingPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SelfHealingPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SelfHealingPolicy{}, &SelfHealingPolicyList{})
}
