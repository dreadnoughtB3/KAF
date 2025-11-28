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
	// Examples:
	//   - rate(tetragon_events_total{event_type="execve"}[1m])
	//   - container_memory_working_set_bytes{pod=~".*"} / container_spec_memory_limit_bytes{pod=~".*"}
	//   - rate(cilium_drop_count_total[2m])
	Query string `json:"query"`

	// Operator for comparison
	// +kubebuilder:validation:Enum=">";"<";"==";">=";"<="
	Operator string `json:"operator"`

	// Threshold value to compare against the query result
	// We use string to avoid float precision issues and allow parsing in controller
	// Example: "50", "0.5", "100.0"
	Threshold string `json:"threshold"`

	// For specifies how long the condition must be true before triggering
	// This prevents flapping and false positives from temporary spikes
	// Example: "1m", "30s", "2m"
	// +kubebuilder:validation:Pattern="^[0-9]+(s|m|h)$"
	// +optional
	For string `json:"for,omitempty"`
}

// RecoveryStep defines a single action in the recovery pipeline
type RecoveryStep struct {
	// Name of the step
	Name string `json:"name"`

	// Action type to perform
	// Available actions:
	//   - DeletePod: Delete the affected pod (ReplicaSet will recreate it)
	//   - CordonNode: Mark the node as unschedulable
	//   - RollingRestart: Trigger a rolling restart of the deployment
	// +kubebuilder:validation:Enum="DeletePod";"CordonNode";"RollingRestart"
	Action string `json:"action"`

	// Params are optional parameters for the action
	// For DeletePod:
	//   - gracePeriod: Grace period in seconds (default: 0 for immediate deletion)
	// Example: {"gracePeriod": "30"}
	// +optional
	Params map[string]string `json:"params,omitempty"`
}

// SelfHealingPolicySpec defines the desired state of SelfHealingPolicy
type SelfHealingPolicySpec struct {
	// Target selects the pods to be monitored and healed
	// Uses standard Kubernetes label selector syntax
	// Example:
	//   matchLabels:
	//     app: frontend
	Target metav1.LabelSelector `json:"target"`

	// CheckInterval determines how often to query Prometheus
	// Default: "15s"
	// +kubebuilder:default="15s"
	// +kubebuilder:validation:Pattern="^[0-9]+(s|m|h)$"
	// +optional
	CheckInterval string `json:"checkInterval,omitempty"`

	// TriggerLogic determines how multiple triggers are combined
	// - OR: Any trigger matching will activate healing (default)
	// - AND: All triggers must match to activate healing
	// +kubebuilder:validation:Enum="OR";"AND"
	// +kubebuilder:default="OR"
	// +optional
	TriggerLogic string `json:"triggerLogic,omitempty"`

	// Triggers is a list of conditions that activate the healing pipeline
	// At least one trigger must be defined
	// +kubebuilder:validation:MinItems=1
	Triggers []TriggerRule `json:"triggers"`

	// Pipeline is the ordered list of recovery actions
	// Actions are executed sequentially
	// +kubebuilder:validation:MinItems=1
	Pipeline []RecoveryStep `json:"pipeline"`

	// CooldownPeriod is the time to wait after a healing pipeline execution
	// before resuming monitoring. This prevents healing loops.
	// Default: "5m"
	// +kubebuilder:default="5m"
	// +kubebuilder:validation:Pattern="^[0-9]+(s|m|h)$"
	// +optional
	CooldownPeriod string `json:"cooldownPeriod,omitempty"`
}

// SelfHealingPolicyStatus defines the observed state of SelfHealingPolicy
type SelfHealingPolicyStatus struct {
	// LastExecutionTime is when the pipeline last ran
	// +optional
	LastExecutionTime *metav1.Time `json:"lastExecutionTime,omitempty"`

	// Phase describes the current state of the policy
	// - Healthy: Monitoring active, no issues detected
	// - Detecting: Condition met but waiting for "For" duration
	// - Healing: Currently executing recovery pipeline
	// - Cooldown: Waiting for cooldown period to expire
	// +kubebuilder:validation:Enum="Healthy";"Detecting";"Healing";"Cooldown"
	// +optional
	Phase string `json:"phase,omitempty"`

	// CurrentPipelineStep indicates which step is currently running (if Phase is Healing)
	// +optional
	CurrentPipelineStep string `json:"currentPipelineStep,omitempty"`

	// TriggeredRule records which rule caused the healing
	// +optional
	TriggeredRule string `json:"triggeredRule,omitempty"`

	// ObservedGeneration reflects the generation of the most recently observed SelfHealingPolicy
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of the policy's state
	// +optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Triggered Rule",type=string,JSONPath=`.status.triggeredRule`
//+kubebuilder:printcolumn:name="Last Execution",type=date,JSONPath=`.status.lastExecutionTime`
//+kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

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
