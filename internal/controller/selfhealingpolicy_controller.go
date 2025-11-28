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

package controller

import (
	"context"
	"fmt"
	"os"
	"strconv"
	//"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	healingv1 "github.com/dreadnoughtB3/KAF/api/v1"

	// Prometheus Client Libraries
	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	prommodel "github.com/prometheus/common/model"

	// Add missing import for labels
	"k8s.io/apimachinery/pkg/labels"
)

// ActionFunc defines the signature for a recovery action
type ActionFunc func(ctx context.Context, r *SelfHealingPolicyReconciler, pod corev1.Pod, params map[string]string) error

// TriggerState tracks the state of a trigger for "For" duration evaluation
type TriggerState struct {
	FirstTriggeredTime *time.Time
	LastCheckedTime    *time.Time
	ConsecutiveHits    int
}

// SelfHealingPolicyReconciler reconciles a SelfHealingPolicy object
type SelfHealingPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	PromAPI  promv1.API

	// ActionRegistry holds available recovery actions
	ActionRegistry map[string]ActionFunc

	// TriggerStateMap tracks trigger states for "For" duration
	// Key format: "namespace/policyName/ruleName/podName"
	TriggerStateMap map[string]*TriggerState
}

//+kubebuilder:rbac:groups=healing.research.io,resources=selfhealingpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=healing.research.io,resources=selfhealingpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete;update;patch
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="apps",resources=deployments,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is the main loop
func (r *SelfHealingPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// 1. Fetch the Policy
	var policy healingv1.SelfHealingPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// 2. Initialize Status if needed
	if policy.Status.Phase == "" {
		policy.Status.Phase = "Healthy"
		if err := r.Status().Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 3. Check Cooldown
	if policy.Status.Phase == "Cooldown" {
		cooldownDur, _ := time.ParseDuration(policy.Spec.CooldownPeriod)
		if policy.Status.LastExecutionTime != nil && time.Since(policy.Status.LastExecutionTime.Time) < cooldownDur {
			// Still in cooldown
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		// Cooldown finished
		policy.Status.Phase = "Healthy"
		policy.Status.CurrentPipelineStep = ""
		policy.Status.TriggeredRule = ""
		if err := r.Status().Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 4. Monitoring Logic
	checkInterval, _ := time.ParseDuration(policy.Spec.CheckInterval)
	if checkInterval == 0 {
		checkInterval = 15 * time.Second
	}

	// Find Target Pods
	var podList corev1.PodList

	selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.Target)
	if err != nil {
		logger.Error(err, "Failed to parse LabelSelector from Policy")
		return ctrl.Result{}, err
	}

	opts := []client.ListOption{
		client.InNamespace(req.Namespace),
		client.MatchingLabelsSelector{Selector: selector},
	}
	if err := r.List(ctx, &podList, opts...); err != nil {
		logger.Error(err, "Failed to list target pods")
		return ctrl.Result{RequeueAfter: checkInterval}, err
	}

	// Check Triggers
	triggered, ruleName, affectedPod := r.evaluateTriggers(ctx, policy, podList)
	if triggered {
		logger.Info("Policy Triggered!", "Rule", ruleName, "Pod", affectedPod.Name)

		// Update Status to Healing
		policy.Status.Phase = "Healing"
		policy.Status.TriggeredRule = ruleName
		now := metav1.Now()
		policy.Status.LastExecutionTime = &now
		if err := r.Status().Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}

		// Execute Pipeline
		r.executePipeline(ctx, &policy, *affectedPod)

		// Move to Cooldown
		policy.Status.Phase = "Cooldown"
		policy.Status.CurrentPipelineStep = "Completed"
		if err := r.Status().Update(ctx, &policy); err != nil {
			logger.Error(err, "Failed to update status to Cooldown")
		}

		// Clear trigger state for this rule
		stateKey := fmt.Sprintf("%s/%s/%s/%s", policy.Namespace, policy.Name, ruleName, affectedPod.Name)
		delete(r.TriggerStateMap, stateKey)
	}

	return ctrl.Result{RequeueAfter: checkInterval}, nil
}

// evaluateTriggers checks all rules against Prometheus
func (r *SelfHealingPolicyReconciler) evaluateTriggers(ctx context.Context, policy healingv1.SelfHealingPolicy, pods corev1.PodList) (bool, string, *corev1.Pod) {
	logger := log.FromContext(ctx)

	triggerLogic := policy.Spec.TriggerLogic
	if triggerLogic == "" {
		triggerLogic = "OR"
	}

	matchedRules := make(map[string]*corev1.Pod)

	for _, rule := range policy.Spec.Triggers {
		// Context timeout for query
		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		val, warnings, err := r.PromAPI.Query(queryCtx, rule.Query, time.Now())
		if err != nil {
			logger.Error(err, "Prometheus query failed", "query", rule.Query)
			continue
		}
		if len(warnings) > 0 {
			logger.Info("Prometheus query warnings", "warnings", warnings)
		}
		logger.Info("Prometheus query result", "rule", rule.Name, "result", val.String())

		// Parse Threshold
		threshold, err := strconv.ParseFloat(rule.Threshold, 64)
		if err != nil {
			logger.Error(err, "Invalid threshold format", "val", rule.Threshold)
			continue
		}

		// Check results based on type
		switch v := val.(type) {
		case prommodel.Vector:
			for _, sample := range v {
				value := float64(sample.Value)
				matched := r.compareValue(value, rule.Operator, threshold)

				if matched {
					// Try to extract pod name from metric labels
					podName := r.extractPodName(sample.Metric)

					// Find the pod in our target list
					var targetPod *corev1.Pod
					for i := range pods.Items {
						if podName != "" && pods.Items[i].Name == podName {
							targetPod = &pods.Items[i]
							break
						}
					}

					// If no specific pod found but rule matched, use first pod in list
					if targetPod == nil && len(pods.Items) > 0 {
						targetPod = &pods.Items[0]
						logger.Info("No specific pod found in metric, using first target pod",
							"rule", rule.Name, "pod", targetPod.Name)
					}

					if targetPod != nil {
						// Check "For" duration if specified
						if rule.For != "" {
							forDuration, err := time.ParseDuration(rule.For)
							if err != nil {
								logger.Error(err, "Invalid 'for' duration", "for", rule.For)
								continue
							}

							stateKey := fmt.Sprintf("%s/%s/%s/%s",
								policy.Namespace, policy.Name, rule.Name, targetPod.Name)

							now := time.Now()
							state, exists := r.TriggerStateMap[stateKey]

							if !exists {
								// First time triggering
								r.TriggerStateMap[stateKey] = &TriggerState{
									FirstTriggeredTime: &now,
									LastCheckedTime:    &now,
									ConsecutiveHits:    1,
								}
								logger.Info("Trigger condition met, starting 'for' duration",
									"rule", rule.Name, "pod", targetPod.Name, "for", rule.For)
								continue
							}

							// Update state
							state.LastCheckedTime = &now
							state.ConsecutiveHits++

							// Check if duration has been satisfied
							if time.Since(*state.FirstTriggeredTime) >= forDuration {
								logger.Info("Trigger 'for' duration satisfied",
									"rule", rule.Name, "pod", targetPod.Name,
									"duration", time.Since(*state.FirstTriggeredTime))
								matchedRules[rule.Name] = targetPod
							} else {
								logger.Info("Trigger still within 'for' duration",
									"rule", rule.Name, "pod", targetPod.Name,
									"elapsed", time.Since(*state.FirstTriggeredTime),
									"required", forDuration)
							}
						} else {
							// No "For" duration, trigger immediately
							matchedRules[rule.Name] = targetPod
						}
					}
				} else {
					// Condition no longer met, clear state
					if podName := r.extractPodName(sample.Metric); podName != "" {
						stateKey := fmt.Sprintf("%s/%s/%s/%s",
							policy.Namespace, policy.Name, rule.Name, podName)
						if _, exists := r.TriggerStateMap[stateKey]; exists {
							logger.Info("Trigger condition no longer met, clearing state",
								"rule", rule.Name, "pod", podName)
							delete(r.TriggerStateMap, stateKey)
						}
					}
				}
			}
		case *prommodel.Scalar:
			value := float64(v.Value)
			matched := r.compareValue(value, rule.Operator, threshold)
			if matched && len(pods.Items) > 0 {
				// Scalar metrics don't have pod labels, use first pod
				matchedRules[rule.Name] = &pods.Items[0]
				logger.Info("Scalar metric matched", "rule", rule.Name, "value", value)
			}
		}
	}

	// Evaluate based on TriggerLogic
	if triggerLogic == "AND" {
		// All rules must match
		if len(matchedRules) == len(policy.Spec.Triggers) {
			// Return the first matched rule
			for ruleName, pod := range matchedRules {
				return true, ruleName, pod
			}
		}
	} else {
		// OR logic (default): any rule matching triggers healing
		if len(matchedRules) > 0 {
			// Return the first matched rule
			for ruleName, pod := range matchedRules {
				return true, ruleName, pod
			}
		}
	}

	return false, "", nil
}

// compareValue compares a value against a threshold using the specified operator
func (r *SelfHealingPolicyReconciler) compareValue(value float64, operator string, threshold float64) bool {
	switch operator {
	case ">":
		return value > threshold
	case "<":
		return value < threshold
	case "==":
		return value == threshold
	case ">=":
		return value >= threshold
	case "<=":
		return value <= threshold
	default:
		return false
	}
}

// extractPodName attempts to extract pod name from metric labels
func (r *SelfHealingPolicyReconciler) extractPodName(metric prommodel.Metric) string {
	// Try common label names for pod identification
	podLabelNames := []string{"pod", "pod_name", "kubernetes_pod_name", "name"}

	for _, labelName := range podLabelNames {
		if podName, ok := metric[prommodel.LabelName(labelName)]; ok {
			return string(podName)
		}
	}

	return ""
}

// executePipeline runs the recovery steps
func (r *SelfHealingPolicyReconciler) executePipeline(ctx context.Context, policy *healingv1.SelfHealingPolicy, pod corev1.Pod) {
	logger := log.FromContext(ctx)

	for i, step := range policy.Spec.Pipeline {
		logger.Info("Executing Pipeline Step",
			"Step", fmt.Sprintf("%d/%d", i+1, len(policy.Spec.Pipeline)),
			"Name", step.Name,
			"Action", step.Action)

		// Update current step in status
		policy.Status.CurrentPipelineStep = step.Name
		if err := r.Status().Update(ctx, policy); err != nil {
			logger.Error(err, "Failed to update pipeline step status")
		}

		// Record Event
		r.Recorder.Event(policy, "Warning", "HealingStarted",
			fmt.Sprintf("Running step '%s' (action: %s) on pod %s", step.Name, step.Action, pod.Name))

		actionFunc, exists := r.ActionRegistry[step.Action]
		if !exists {
			logger.Error(nil, "Unknown action", "action", step.Action)
			r.Recorder.Event(policy, "Warning", "ActionFailed",
				fmt.Sprintf("Unknown action: %s", step.Action))
			continue
		}

		if err := actionFunc(ctx, r, pod, step.Params); err != nil {
			logger.Error(err, "Action failed", "step", step.Name, "action", step.Action)
			r.Recorder.Event(policy, "Warning", "ActionFailed",
				fmt.Sprintf("Step '%s' failed: %v", step.Name, err))
			// Continue to next step even on failure
		} else {
			logger.Info("Action succeeded", "step", step.Name, "action", step.Action)
			r.Recorder.Event(policy, "Normal", "ActionSucceeded",
				fmt.Sprintf("Step '%s' completed successfully", step.Name))
		}

		// Add small delay between steps
		time.Sleep(2 * time.Second)
	}
}

// --- Action Implementations ---

func actionDeletePod(ctx context.Context, r *SelfHealingPolicyReconciler, pod corev1.Pod, params map[string]string) error {
	logger := log.FromContext(ctx)

	grace := int64(0) // Default force delete
	if gracePeriod, ok := params["gracePeriod"]; ok {
		if g, err := strconv.ParseInt(gracePeriod, 10, 64); err == nil {
			grace = g
		}
	}

	deleteOpts := []client.DeleteOption{client.GracePeriodSeconds(grace)}
	if err := r.Delete(ctx, &pod, deleteOpts...); err != nil {
		return fmt.Errorf("failed to delete pod %s: %w", pod.Name, err)
	}

	logger.Info("Pod deleted successfully", "pod", pod.Name, "gracePeriod", grace)
	return nil
}

func actionCordonNode(ctx context.Context, r *SelfHealingPolicyReconciler, pod corev1.Pod, params map[string]string) error {
	logger := log.FromContext(ctx)

	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		return fmt.Errorf("pod is not scheduled on any node")
	}

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	if node.Spec.Unschedulable {
		logger.Info("Node is already cordoned", "node", nodeName)
		return nil
	}

	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = true
	if err := r.Patch(ctx, &node, patch); err != nil {
		return fmt.Errorf("failed to cordon node %s: %w", nodeName, err)
	}

	logger.Info("Node cordoned successfully", "node", nodeName)
	return nil
}

func actionRollingRestart(ctx context.Context, r *SelfHealingPolicyReconciler, pod corev1.Pod, params map[string]string) error {
	logger := log.FromContext(ctx)

	// Try to find deployment from pod's owner references
	var deployment *appsv1.Deployment

	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "ReplicaSet" {
			var rs appsv1.ReplicaSet
			if err := r.Get(ctx, client.ObjectKey{
				Namespace: pod.Namespace,
				Name:      owner.Name,
			}, &rs); err == nil {
				// ReplicaSet found, now find its Deployment owner
				for _, rsOwner := range rs.OwnerReferences {
					if rsOwner.Kind == "Deployment" {
						var dep appsv1.Deployment
						if err := r.Get(ctx, client.ObjectKey{
							Namespace: pod.Namespace,
							Name:      rsOwner.Name,
						}, &dep); err == nil {
							deployment = &dep
							break
						}
					}
				}
			}
		}
	}

	// Fallback: try to find by app label
	if deployment == nil {
		// appName, ok := pod.Labels["app"]
		_, ok := pod.Labels["app"]
		if !ok {
			// Try alternative label
			// appName, ok = pod.Labels["app.kubernetes.io/name"]
			_, ok = pod.Labels["app.kubernetes.io/name"]
			if !ok {
				return fmt.Errorf("pod has no 'app' or 'app.kubernetes.io/name' label")
			}
		}

		var deployList appsv1.DeploymentList
		listOpts := []client.ListOption{
			client.InNamespace(pod.Namespace),
		}

		if err := r.List(ctx, &deployList, listOpts...); err != nil {
			return fmt.Errorf("failed to list deployments: %w", err)
		}

		// Find deployment with matching selector
		for i := range deployList.Items {
			if deployList.Items[i].Spec.Selector != nil {
				selector, err := metav1.LabelSelectorAsSelector(deployList.Items[i].Spec.Selector)
				if err != nil {
					continue
				}
				if selector.Matches(labels.Set(pod.Labels)) {
					deployment = &deployList.Items[i]
					break
				}
			}
		}
	}

	if deployment == nil {
		return fmt.Errorf("no deployment found for pod")
	}

	// Trigger rolling restart by updating annotation
	patch := client.MergeFrom(deployment.DeepCopy())
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}
	deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)
	deployment.Spec.Template.Annotations["healing.research.io/restartedBy"] = "selfhealingpolicy"

	if err := r.Patch(ctx, deployment, patch); err != nil {
		return fmt.Errorf("failed to trigger rolling restart for deployment %s: %w", deployment.Name, err)
	}

	logger.Info("Triggered Rolling Restart", "deployment", deployment.Name, "namespace", deployment.Namespace)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SelfHealingPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// 1. Initialize Prometheus Client
	promAddr := os.Getenv("PROMETHEUS_ADDRESS")
	if promAddr == "" {
		promAddr = "http://prometheus-kube-prometheus-prometheus.monitoring:9090"
	}

	client, err := promapi.NewClient(promapi.Config{Address: promAddr})
	if err != nil {
		return fmt.Errorf("failed to create Prometheus client: %w", err)
	}
	r.PromAPI = promv1.NewAPI(client)

	// 2. Register Actions
	r.ActionRegistry = map[string]ActionFunc{
		"DeletePod":      actionDeletePod,
		"CordonNode":     actionCordonNode,
		"RollingRestart": actionRollingRestart,
	}

	// 3. Initialize TriggerStateMap
	r.TriggerStateMap = make(map[string]*TriggerState)

	r.Recorder = mgr.GetEventRecorderFor("selfhealingpolicy-controller")

	return ctrl.NewControllerManagedBy(mgr).
		For(&healingv1.SelfHealingPolicy{}).
		Complete(r)
}
