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
)

// ActionFunc defines the signature for a recovery action
type ActionFunc func(ctx context.Context, r *SelfHealingPolicyReconciler, pod corev1.Pod, params map[string]string) error

// SelfHealingPolicyReconciler reconciles a SelfHealingPolicy object
type SelfHealingPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	PromAPI  promv1.API

	// ActionRegistry holds available recovery actions
	ActionRegistry map[string]ActionFunc
}

//+kubebuilder:rbac:groups=healing.research.io,resources=selfhealingpolicies,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=healing.research.io,resources=selfhealingpolicies/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete;update;patch
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

	// 1. LabelSelector (Struct) を labels.Selector (Interface) に変換する
	selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.Target)
	if err != nil {
		logger.Error(err, "Failed to parse LabelSelector from Policy")
		// セレクタの定義が不正な場合はリトライしても直らないので、エラーログを出して終了させる手もあるが、
		// ここではStatus更新などは省略してエラーを返す
		return ctrl.Result{}, err
	}

	// 2. 変換したセレクタを使ってPodを検索する
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
	}

	return ctrl.Result{RequeueAfter: checkInterval}, nil
}

// evaluateTriggers checks all rules against Prometheus
func (r *SelfHealingPolicyReconciler) evaluateTriggers(ctx context.Context, policy healingv1.SelfHealingPolicy, pods corev1.PodList) (bool, string, *corev1.Pod) {
	logger := log.FromContext(ctx)

	for _, rule := range policy.Spec.Triggers {
		// Run PromQL
		// Context timeout for query
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		val, warnings, err := r.PromAPI.Query(ctx, rule.Query, time.Now())
		if err != nil {
			logger.Error(err, "Prometheus query failed", "query", rule.Query)
			continue
		}
		if len(warnings) > 0 {
			logger.Info("Prometheus query warnings", "warnings", warnings)
		}
		// Logging the raw result for debugging
		logger.Info("Prometheus query result", "result", val.String())

		// Parse Threshold
		threshold, err := strconv.ParseFloat(rule.Threshold, 64)
		if err != nil {
			logger.Error(err, "Invalid threshold format", "val", rule.Threshold)
			continue
		}

		// Check Vector results
		if vector, ok := val.(prommodel.Vector); ok {
			for _, sample := range vector {
				value := float64(sample.Value)
				matched := false

				// Compare based on operator
				switch rule.Operator {
				case ">":
					matched = value > threshold
				case "<":
					matched = value < threshold
				case "==":
					matched = value == threshold
				}

				if matched {
					// Map result back to a Pod
					// We assume the metrics have a 'pod' label (standard in K8s)
					podName := string(sample.Metric["pod"])

					// Verify this pod is in our target list
					for _, p := range pods.Items {
						if p.Name == podName {
							return true, rule.Name, &p
						}
					}
				}
			}
		}
	}
	return false, "", nil
}

// executePipeline runs the recovery steps
func (r *SelfHealingPolicyReconciler) executePipeline(ctx context.Context, policy *healingv1.SelfHealingPolicy, pod corev1.Pod) {
	logger := log.FromContext(ctx)

	for _, step := range policy.Spec.Pipeline {
		logger.Info("Executing Pipeline Step", "Step", step.Name, "Action", step.Action)

		// Record Event
		r.Recorder.Event(policy, "Warning", "HealingStarted", fmt.Sprintf("Running step %s on pod %s", step.Name, pod.Name))

		actionFunc, exists := r.ActionRegistry[step.Action]
		if !exists {
			logger.Error(nil, "Unknown action", "action", step.Action)
			continue
		}

		if err := actionFunc(ctx, r, pod, step.Params); err != nil {
			logger.Error(err, "Action failed", "step", step.Name)
			// Continue or break based on policy? For now, we continue.
		}
	}
}

// --- Action Implementations (The "Hands") ---

func actionDeletePod(ctx context.Context, r *SelfHealingPolicyReconciler, pod corev1.Pod, params map[string]string) error {
	grace := int64(0) // Default force delete
	deleteOpts := []client.DeleteOption{client.GracePeriodSeconds(grace)}
	return r.Delete(ctx, &pod, deleteOpts...)
}

func actionCordonNode(ctx context.Context, r *SelfHealingPolicyReconciler, pod corev1.Pod, params map[string]string) error {
	logger := log.FromContext(ctx)

	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		return fmt.Errorf("pod is not scheduled on any node")
	}

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
		return err
	}

	// 既にCordonされていたら何もしない
	if node.Spec.Unschedulable {
		logger.Info("Node is already cordoned", "node", nodeName)
		return nil
	}

	// パッチを作成して適用
	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = true
	if err := r.Patch(ctx, &node, patch); err != nil {
		return err
	}

	logger.Info("Node cordoned successfully", "node", nodeName)
	return nil
}

// Action: RollingRestart
// DeploymentのPodTemplateにアノテーションを付与して、全Podの再作成をトリガーする
func actionRollingRestart(ctx context.Context, r *SelfHealingPolicyReconciler, pod corev1.Pod, params map[string]string) error {
	logger := log.FromContext(ctx)

	// PodのラベルからDeploymentを特定（簡易実装）
	appName, ok := pod.Labels["app"]
	if !ok {
		return fmt.Errorf("pod has no 'app' label")
	}
	var deployList appsv1.DeploymentList
	if err := r.List(ctx, &deployList, client.InNamespace(pod.Namespace), client.MatchingLabels{"app": appName}); err != nil {
		return err
	}
	if len(deployList.Items) == 0 {
		return fmt.Errorf("no deployment found")
	}
	deployment := deployList.Items[0]

	// アノテーション "kubectl.kubernetes.io/restartedAt" を更新してRolloutを強制
	patch := client.MergeFrom(deployment.DeepCopy())
	if deployment.Spec.Template.Annotations == nil {
		deployment.Spec.Template.Annotations = make(map[string]string)
	}
	deployment.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

	if err := r.Patch(ctx, &deployment, patch); err != nil {
		return err
	}
	logger.Info("Triggered Rolling Restart", "deployment", deployment.Name)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SelfHealingPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// 1. Initialize Prometheus Client
	// Use environment variable for flexibility, default to in-cluster DNS
	promAddr := os.Getenv("PROMETHEUS_ADDRESS")
	if promAddr == "" {
		promAddr = "http://prometheus-kube-prometheus-prometheus.monitoring:9090"
	}

	client, err := promapi.NewClient(promapi.Config{Address: promAddr})
	if err != nil {
		return err
	}
	r.PromAPI = promv1.NewAPI(client)

	// 2. Register Actions
	r.ActionRegistry = map[string]ActionFunc{
		"DeletePod":      actionDeletePod,
		"CordonNode":     actionCordonNode,
		"RollingRestart": actionRollingRestart,
		// Add more actions here later (e.g., RollbackDeployment)
	}

	r.Recorder = mgr.GetEventRecorderFor("selfhealingpolicy-controller")

	return ctrl.NewControllerManagedBy(mgr).
		For(&healingv1.SelfHealingPolicy{}).
		Complete(r)
}
