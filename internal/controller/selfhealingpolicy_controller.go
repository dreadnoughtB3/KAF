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

// internal/controller/selfhealingpolicy_controller.go

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
	"time"

	"github.com/fatih/color"
	"github.com/go-logr/logr" // ← これを追加

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	healingv1 "github.com/dreadnoughtB3/KAF/api/v1"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	prommodel "github.com/prometheus/common/model"

	"k8s.io/apimachinery/pkg/labels"
)

// カラー定義
var (
	colorInfo      = color.New(color.FgCyan, color.Bold)
	colorSuccess   = color.New(color.FgGreen, color.Bold)
	colorWarning   = color.New(color.FgYellow, color.Bold)
	colorError     = color.New(color.FgRed, color.Bold)
	colorCritical  = color.New(color.FgRed, color.Bold, color.BgWhite)
	colorDebug     = color.New(color.FgWhite)
	colorHighlight = color.New(color.FgMagenta, color.Bold)
)

// ログヘルパー関数
func logInfo(logger logr.Logger, msg string, keysAndValues ...interface{}) {
	fmt.Println(colorInfo.Sprint("ℹ INFO: "), msg)
	logger.Info(msg, keysAndValues...)
}

func logSuccess(logger logr.Logger, msg string, keysAndValues ...interface{}) {
	fmt.Println(colorSuccess.Sprint("✓ SUCCESS: "), msg)
	logger.Info(msg, keysAndValues...)
}

func logWarning(logger logr.Logger, msg string, keysAndValues ...interface{}) {
	fmt.Println(colorWarning.Sprint("⚠ WARNING: "), msg)
	logger.Info(msg, keysAndValues...)
}

func logError(logger logr.Logger, err error, msg string, keysAndValues ...interface{}) {
	if err != nil {
		fmt.Println(colorError.Sprint("✗ ERROR: "), msg, "-", err)
		logger.Error(err, msg, keysAndValues...)
	} else {
		fmt.Println(colorError.Sprint("✗ ERROR: "), msg)
		logger.Info(msg, keysAndValues...)
	}
}

func logCritical(logger logr.Logger, msg string, keysAndValues ...interface{}) {
	fmt.Println(colorCritical.Sprint("🔥 CRITICAL: "), msg)
	logger.Info(msg, keysAndValues...)
}

func logDebug(logger logr.Logger, msg string, keysAndValues ...interface{}) {
	fmt.Println(colorDebug.Sprint("• DEBUG: "), msg)
	logger.V(1).Info(msg, keysAndValues...)
}

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

	ActionRegistry  map[string]ActionFunc
	TriggerStateMap map[string]*TriggerState
}

//+kubebuilder:rbac:groups=healing.kaf.io,resources=selfhealingpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=healing.kaf.io,resources=selfhealingpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete;update;patch
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="apps",resources=deployments,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *SelfHealingPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var policy healingv1.SelfHealingPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// logInfo(logger, "Reconciling SelfHealingPolicy",
	// 	"policy", req.NamespacedName.Name,
	// 	"namespace", req.Namespace)

	if policy.Status.Phase == "" {
		policy.Status.Phase = "Healthy"
		if err := r.Status().Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
		logSuccess(logger, "Initialized policy status", "phase", "Healthy")
	}

	if policy.Status.Phase == "Cooldown" {
		cooldownDur, _ := time.ParseDuration(policy.Spec.CooldownPeriod)
		if policy.Status.LastExecutionTime != nil && time.Since(policy.Status.LastExecutionTime.Time) < cooldownDur {
			remaining := cooldownDur - time.Since(policy.Status.LastExecutionTime.Time)
			logDebug(logger, "Policy in cooldown",
				"remaining", remaining.Round(time.Second).String())
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}

		policy.Status.Phase = "Healthy"
		policy.Status.CurrentPipelineStep = ""
		policy.Status.TriggeredRule = ""
		if err := r.Status().Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}
		logSuccess(logger, "Cooldown period finished", "newPhase", "Healthy")
	}

	checkInterval, _ := time.ParseDuration(policy.Spec.CheckInterval)
	if checkInterval == 0 {
		checkInterval = 15 * time.Second
	}

	var podList corev1.PodList
	selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.Target)
	if err != nil {
		logError(logger, err, "Failed to parse LabelSelector from Policy")
		return ctrl.Result{}, err
	}

	opts := []client.ListOption{
		client.InNamespace(req.Namespace),
		client.MatchingLabelsSelector{Selector: selector},
	}
	if err := r.List(ctx, &podList, opts...); err != nil {
		logError(logger, err, "Failed to list target pods")
		return ctrl.Result{RequeueAfter: checkInterval}, err
	}

	// logDebug(logger, "Found target pods", "count", len(podList.Items))

	triggered, ruleName, affectedPod := r.evaluateTriggers(ctx, policy, podList)
	if triggered {
		logCritical(logger, "🚨 POLICY TRIGGERED!",
			"rule", ruleName,
			"pod", affectedPod.Name,
			"namespace", affectedPod.Namespace)

		policy.Status.Phase = "Healing"
		policy.Status.TriggeredRule = ruleName
		now := metav1.Now()
		policy.Status.LastExecutionTime = &now
		if err := r.Status().Update(ctx, &policy); err != nil {
			return ctrl.Result{}, err
		}

		r.executePipeline(ctx, &policy, *affectedPod)

		policy.Status.Phase = "Cooldown"
		policy.Status.CurrentPipelineStep = "Completed"
		if err := r.Status().Update(ctx, &policy); err != nil {
			logError(logger, err, "Failed to update status to Cooldown")
		}

		stateKey := fmt.Sprintf("%s/%s/%s/%s", policy.Namespace, policy.Name, ruleName, affectedPod.Name)
		delete(r.TriggerStateMap, stateKey)

		logSuccess(logger, "Healing pipeline completed", "cooldownPeriod", policy.Spec.CooldownPeriod)
	}

	return ctrl.Result{RequeueAfter: checkInterval}, nil
}

func (r *SelfHealingPolicyReconciler) evaluateTriggers(ctx context.Context, policy healingv1.SelfHealingPolicy, pods corev1.PodList) (bool, string, *corev1.Pod) {
	logger := log.FromContext(ctx)

	triggerLogic := policy.Spec.TriggerLogic
	if triggerLogic == "" {
		triggerLogic = "OR"
	}

	matchedRules := make(map[string]*corev1.Pod)

	for _, rule := range policy.Spec.Triggers {
		queryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		val, warnings, err := r.PromAPI.Query(queryCtx, rule.Query, time.Now())
		if err != nil {
			logError(logger, err, "Prometheus query failed", "query", rule.Query)
			continue
		}
		if len(warnings) > 0 {
			logWarning(logger, "Prometheus query warnings", "warnings", warnings)
		}

		// fmt.Println(colorDebug.Sprint("📊 Query Result:"),
		// 	colorHighlight.Sprintf("[%s]", rule.Name),
		// 	"=", val.String())

		threshold, err := strconv.ParseFloat(rule.Threshold, 64)
		if err != nil {
			logError(logger, err, "Invalid threshold format", "val", rule.Threshold)
			continue
		}

		switch v := val.(type) {
		case prommodel.Vector:
			for _, sample := range v {
				value := float64(sample.Value)
				matched := r.compareValue(value, rule.Operator, threshold)

				if matched {
					fmt.Println(colorWarning.Sprint("⚡ Condition Matched:"),
						colorHighlight.Sprintf("%s %s %s",
							fmt.Sprintf("%.2f", value),
							rule.Operator,
							rule.Threshold))

					podName := r.extractPodName(sample.Metric)
					var targetPod *corev1.Pod
					for i := range pods.Items {
						if podName != "" && pods.Items[i].Name == podName {
							targetPod = &pods.Items[i]
							break
						}
					}

					if targetPod == nil && len(pods.Items) > 0 {
						targetPod = &pods.Items[0]
						logDebug(logger, "No specific pod found in metric, using first target pod",
							"rule", rule.Name, "pod", targetPod.Name)
					}

					if targetPod != nil {
						if rule.For != "" {
							forDuration, err := time.ParseDuration(rule.For)
							if err != nil {
								logError(logger, err, "Invalid 'for' duration", "for", rule.For)
								continue
							}

							stateKey := fmt.Sprintf("%s/%s/%s/%s",
								policy.Namespace, policy.Name, rule.Name, targetPod.Name)

							now := time.Now()
							state, exists := r.TriggerStateMap[stateKey]

							if !exists {
								r.TriggerStateMap[stateKey] = &TriggerState{
									FirstTriggeredTime: &now,
									LastCheckedTime:    &now,
									ConsecutiveHits:    1,
								}
								logInfo(logger, "⏱️  Trigger condition met, starting 'for' duration",
									"rule", rule.Name,
									"pod", targetPod.Name,
									"for", rule.For)
								continue
							}

							state.LastCheckedTime = &now
							state.ConsecutiveHits++

							elapsed := time.Since(*state.FirstTriggeredTime)
							if elapsed >= forDuration {
								logCritical(logger, "🔥 Trigger 'for' duration SATISFIED",
									"rule", rule.Name,
									"pod", targetPod.Name,
									"duration", elapsed.Round(time.Second).String())
								matchedRules[rule.Name] = targetPod
							} else {
								fmt.Println(colorInfo.Sprint("⏳ Waiting for 'for' duration:"),
									colorHighlight.Sprintf("%s / %s",
										elapsed.Round(time.Second),
										forDuration))
							}
						} else {
							matchedRules[rule.Name] = targetPod
						}
					}
				} else {
					if podName := r.extractPodName(sample.Metric); podName != "" {
						stateKey := fmt.Sprintf("%s/%s/%s/%s",
							policy.Namespace, policy.Name, rule.Name, podName)
						if _, exists := r.TriggerStateMap[stateKey]; exists {
							logDebug(logger, "Trigger condition no longer met, clearing state",
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
				matchedRules[rule.Name] = &pods.Items[0]
				logWarning(logger, "Scalar metric matched", "rule", rule.Name, "value", value)
			}
		}
	}

	if triggerLogic == "AND" {
		if len(matchedRules) == len(policy.Spec.Triggers) {
			for ruleName, pod := range matchedRules {
				return true, ruleName, pod
			}
		}
	} else {
		if len(matchedRules) > 0 {
			for ruleName, pod := range matchedRules {
				return true, ruleName, pod
			}
		}
	}

	return false, "", nil
}

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

func (r *SelfHealingPolicyReconciler) extractPodName(metric prommodel.Metric) string {
	podLabelNames := []string{"pod", "pod_name", "kubernetes_pod_name", "name"}

	for _, labelName := range podLabelNames {
		if podName, ok := metric[prommodel.LabelName(labelName)]; ok {
			return string(podName)
		}
	}

	return ""
}

func (r *SelfHealingPolicyReconciler) executePipeline(ctx context.Context, policy *healingv1.SelfHealingPolicy, pod corev1.Pod) {
	logger := log.FromContext(ctx)

	fmt.Println(colorCritical.Sprint("═══════════════════════════════════════"))
	fmt.Println(colorCritical.Sprint("    EXECUTING HEALING PIPELINE"))
	fmt.Println(colorCritical.Sprint("═══════════════════════════════════════"))

	for i, step := range policy.Spec.Pipeline {
		fmt.Println(colorInfo.Sprintf("\n🔧 Step %d/%d: %s",
			i+1, len(policy.Spec.Pipeline), step.Name))
		fmt.Println(colorDebug.Sprintf("   Action: %s", step.Action))

		policy.Status.CurrentPipelineStep = step.Name
		if err := r.Status().Update(ctx, policy); err != nil {
			logError(logger, err, "Failed to update pipeline step status")
		}

		r.Recorder.Event(policy, "Warning", "HealingStarted",
			fmt.Sprintf("Running step '%s' (action: %s) on pod %s", step.Name, step.Action, pod.Name))

		actionFunc, exists := r.ActionRegistry[step.Action]
		if !exists {
			logError(logger, nil, "Unknown action", "action", step.Action)
			r.Recorder.Event(policy, "Warning", "ActionFailed",
				fmt.Sprintf("Unknown action: %s", step.Action))
			continue
		}

		if err := actionFunc(ctx, r, pod, step.Params); err != nil {
			logError(logger, err, "Action failed", "step", step.Name, "action", step.Action)
			r.Recorder.Event(policy, "Warning", "ActionFailed",
				fmt.Sprintf("Step '%s' failed: %v", step.Name, err))
		} else {
			logSuccess(logger, "Action succeeded", "step", step.Name, "action", step.Action)
			r.Recorder.Event(policy, "Normal", "ActionSucceeded",
				fmt.Sprintf("Step '%s' completed successfully", step.Name))
		}

		time.Sleep(2 * time.Second)
	}

	fmt.Println(colorSuccess.Sprint("\n═══════════════════════════════════════"))
	fmt.Println(colorSuccess.Sprint("    PIPELINE COMPLETED"))
	fmt.Println(colorSuccess.Sprint("═══════════════════════════════════════\n"))
}

// --- Action Implementations ---

func actionDeletePod(ctx context.Context, r *SelfHealingPolicyReconciler, pod corev1.Pod, params map[string]string) error {
	logger := log.FromContext(ctx)

	grace := int64(0)
	if gracePeriod, ok := params["gracePeriod"]; ok {
		if g, err := strconv.ParseInt(gracePeriod, 10, 64); err == nil {
			grace = g
		}
	}

	fmt.Println(colorWarning.Sprintf("   🗑️  Deleting pod: %s (grace period: %ds)", pod.Name, grace))

	deleteOpts := []client.DeleteOption{client.GracePeriodSeconds(grace)}
	if err := r.Delete(ctx, &pod, deleteOpts...); err != nil {
		return fmt.Errorf("failed to delete pod %s: %w", pod.Name, err)
	}

	logSuccess(logger, "Pod deleted successfully", "pod", pod.Name, "gracePeriod", grace)
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
		logInfo(logger, "Node is already cordoned", "node", nodeName)
		return nil
	}

	fmt.Println(colorWarning.Sprintf("   🚧 Cordoning node: %s", nodeName))

	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = true
	if err := r.Patch(ctx, &node, patch); err != nil {
		return fmt.Errorf("failed to cordon node %s: %w", nodeName, err)
	}

	logSuccess(logger, "Node cordoned successfully", "node", nodeName)
	return nil
}

func actionRollingRestart(ctx context.Context, r *SelfHealingPolicyReconciler, pod corev1.Pod, params map[string]string) error {
	logger := log.FromContext(ctx)

	var deployment *appsv1.Deployment

	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "ReplicaSet" {
			var rs appsv1.ReplicaSet
			if err := r.Get(ctx, client.ObjectKey{
				Namespace: pod.Namespace,
				Name:      owner.Name,
			}, &rs); err == nil {
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

	if deployment == nil {
		_, ok := pod.Labels["app"]
		if !ok {
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

	fmt.Println(colorWarning.Sprintf("   🔄 Triggering rolling restart: %s", deployment.Name))

	patch := client.MergeFrom(deployment.DeepCopy())
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}
	deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)
	deployment.Spec.Template.Annotations["healing.kaf.io/restartedBy"] = "selfhealingpolicy"

	if err := r.Patch(ctx, deployment, patch); err != nil {
		return fmt.Errorf("failed to trigger rolling restart for deployment %s: %w", deployment.Name, err)
	}

	logSuccess(logger, "Triggered Rolling Restart", "deployment", deployment.Name, "namespace", deployment.Namespace)
	return nil
}

func (r *SelfHealingPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	promAddr := os.Getenv("PROMETHEUS_ADDRESS")
	if promAddr == "" {
		promAddr = "http://prometheus-kube-prometheus-prometheus.monitoring:9090"
	}

	client, err := promapi.NewClient(promapi.Config{Address: promAddr})
	if err != nil {
		return fmt.Errorf("failed to create Prometheus client: %w", err)
	}
	r.PromAPI = promv1.NewAPI(client)

	r.ActionRegistry = map[string]ActionFunc{
		"DeletePod":      actionDeletePod,
		"CordonNode":     actionCordonNode,
		"RollingRestart": actionRollingRestart,
	}

	r.TriggerStateMap = make(map[string]*TriggerState)

	r.Recorder = mgr.GetEventRecorderFor("selfhealingpolicy-controller")

	fmt.Println(colorSuccess.Sprint("✓ SelfHealingPolicy Controller initialized"))
	fmt.Println(colorInfo.Sprintf("   Prometheus: %s", promAddr))

	return ctrl.NewControllerManagedBy(mgr).
		For(&healingv1.SelfHealingPolicy{}).
		Complete(r)
}
