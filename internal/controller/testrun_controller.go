package controller

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	jmeterv1 "jmeter-controller/api/v1"
	"jmeter-controller/internal/config"
)

const (
	finalizerName       = "jmeter.jmeter.io/finalizer"
	labelTestRun        = "jmeter.jmeter.io/testrun"
	labelRunGroup       = "jmeter.jmeter.io/rungroup"
	defaultBase   int32 = 50
)

// TestRunReconciler reconciles a TestRun object
type TestRunReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config *config.ControllerConfig
}

// +kubebuilder:rbac:groups=jmeter.jmeter.io,resources=testruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=jmeter.jmeter.io,resources=testruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=jmeter.jmeter.io,resources=testruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete

func (r *TestRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the TestRun
	testRun := &jmeterv1.TestRun{}
	if err := r.Get(ctx, req.NamespacedName, testRun); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion
	if !testRun.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, testRun)
	}

	// Ensure finalizer is present
	if !controllerutil.ContainsFinalizer(testRun, finalizerName) {
		controllerutil.AddFinalizer(testRun, finalizerName)
		if err := r.Update(ctx, testRun); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Skip reconcile if already Completed or Failed (unless pods are still around)
	if testRun.Status.Phase == jmeterv1.TestRunPhaseCompleted || testRun.Status.Phase == jmeterv1.TestRunPhaseFailed {
		return ctrl.Result{}, nil
	}

	// Check concurrent limits per run group
	waiting, waitMsg, err := r.checkConcurrentLimits(ctx, testRun)
	if err != nil {
		return ctrl.Result{}, err
	}
	if waiting {
		logger.Info("TestRun is waiting due to concurrent limits", "reason", waitMsg)
		return r.setPhase(ctx, testRun, jmeterv1.TestRunPhaseWaiting, waitMsg, ctrl.Result{RequeueAfter: 30 * time.Second})
	}

	// Ensure all pods exist
	if err := r.reconcilePods(ctx, testRun); err != nil {
		return ctrl.Result{}, err
	}

	// Update status based on observed pod states
	return r.updateStatus(ctx, testRun)
}

// handleDeletion deletes all owned pods and removes the finalizer.
func (r *TestRunReconciler) handleDeletion(ctx context.Context, testRun *jmeterv1.TestRun) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(testRun, finalizerName) {
		return ctrl.Result{}, nil
	}

	if err := r.deleteOwnedPods(ctx, testRun); err != nil {
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(testRun, finalizerName)
	if err := r.Update(ctx, testRun); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// checkConcurrentLimits returns true if this TestRun should wait because the
// concurrent run limit for at least one of its run groups has been reached.
func (r *TestRunReconciler) checkConcurrentLimits(ctx context.Context, testRun *jmeterv1.TestRun) (bool, string, error) {
	// List all TestRuns in the same namespace
	allRuns := &jmeterv1.TestRunList{}
	if err := r.List(ctx, allRuns, client.InNamespace(testRun.Namespace)); err != nil {
		return false, "", err
	}

	// Count running TestRuns per run group (excluding self)
	runningPerGroup := make(map[string]int32)
	for i := range allRuns.Items {
		other := &allRuns.Items[i]
		if other.UID == testRun.UID {
			continue
		}
		if other.Status.Phase != jmeterv1.TestRunPhaseRunning {
			continue
		}
		for groupName := range other.Spec.RunGroups {
			runningPerGroup[groupName]++
		}
	}

	for groupName := range testRun.Spec.RunGroups {
		limit := r.Config.MaxConcurrentForGroup(groupName)
		if runningPerGroup[groupName] >= limit {
			return true, fmt.Sprintf("run group %q has reached its concurrent limit of %d", groupName, limit), nil
		}
	}
	return false, "", nil
}

// reconcilePods ensures that the desired pods exist for every run group.
func (r *TestRunReconciler) reconcilePods(ctx context.Context, testRun *jmeterv1.TestRun) error {
	logger := log.FromContext(ctx)

	existingPods, err := r.listOwnedPods(ctx, testRun)
	if err != nil {
		return err
	}

	// Index existing pods by name for quick lookup
	existingByName := make(map[string]*corev1.Pod, len(existingPods))
	for i := range existingPods {
		existingByName[existingPods[i].Name] = &existingPods[i]
	}

	for groupName, groupCfg := range testRun.Spec.RunGroups {
		base := groupCfg.Base
		if base <= 0 {
			base = defaultBase
		}
		podCount := int(math.Ceil(float64(groupCfg.Thread) / float64(base)))

		for idx := 0; idx < podCount; idx++ {
			podName := podName(testRun.Name, groupName, idx)
			if _, exists := existingByName[podName]; exists {
				continue
			}

			threadCount := base
			// Last pod gets the remainder
			remainder := groupCfg.Thread % base
			if remainder != 0 && idx == podCount-1 {
				threadCount = remainder
			}

			pod := r.buildPod(testRun, groupName, podName, threadCount, groupCfg.NodeSelector)
			if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("creating pod %s: %w", podName, err)
			}
			logger.Info("Created pod", "pod", podName, "runGroup", groupName, "threads", threadCount)
		}
	}
	return nil
}

// updateStatus lists all owned pods and updates TestRun.Status accordingly.
func (r *TestRunReconciler) updateStatus(ctx context.Context, testRun *jmeterv1.TestRun) (ctrl.Result, error) {
	pods, err := r.listOwnedPods(ctx, testRun)
	if err != nil {
		return ctrl.Result{}, err
	}

	podInfos := make([]jmeterv1.PodInfo, 0, len(pods))
	for _, pod := range pods {
		podInfos = append(podInfos, jmeterv1.PodInfo{
			Name:        pod.Name,
			IP:          pod.Status.PodIP,
			RunGroup:    pod.Labels[labelRunGroup],
			ThreadCount: podThreadCount(&pod),
			Phase:       pod.Status.Phase,
		})
	}

	phase, message := computePhase(testRun, pods)
	patch := client.MergeFrom(testRun.DeepCopy())
	testRun.Status.Pods = podInfos
	testRun.Status.Phase = phase
	testRun.Status.Message = message
	if phase == jmeterv1.TestRunPhaseRunning && testRun.Status.StartTime == nil {
		now := metav1.Now()
		testRun.Status.StartTime = &now
	}

	if err := r.Status().Patch(ctx, testRun, patch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// computePhase determines the overall TestRun phase from the owned pods.
func computePhase(testRun *jmeterv1.TestRun, pods []corev1.Pod) (jmeterv1.TestRunPhase, string) {
	// Calculate expected total pod count
	expectedCount := 0
	for _, g := range testRun.Spec.RunGroups {
		base := g.Base
		if base <= 0 {
			base = defaultBase
		}
		expectedCount += int(math.Ceil(float64(g.Thread) / float64(base)))
	}

	if len(pods) == 0 {
		return jmeterv1.TestRunPhasePending, "Waiting for pods to start"
	}

	allDone := true
	anyFailed := false
	for _, pod := range pods {
		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			// done
		case corev1.PodFailed:
			anyFailed = true
		default:
			allDone = false
		}
	}

	if allDone && len(pods) >= expectedCount {
		if anyFailed {
			return jmeterv1.TestRunPhaseFailed, "One or more pods failed"
		}
		return jmeterv1.TestRunPhaseCompleted, "All pods completed successfully"
	}
	return jmeterv1.TestRunPhaseRunning, fmt.Sprintf("%d/%d pods running", len(pods), expectedCount)
}

// setPhase patches only the Status.Phase and Status.Message fields.
func (r *TestRunReconciler) setPhase(ctx context.Context, testRun *jmeterv1.TestRun, phase jmeterv1.TestRunPhase, message string, result ctrl.Result) (ctrl.Result, error) {
	patch := client.MergeFrom(testRun.DeepCopy())
	testRun.Status.Phase = phase
	testRun.Status.Message = message
	if err := r.Status().Patch(ctx, testRun, patch); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

// listOwnedPods returns all pods that are owned by this TestRun.
func (r *TestRunReconciler) listOwnedPods(ctx context.Context, testRun *jmeterv1.TestRun) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(testRun.Namespace),
		client.MatchingLabels{labelTestRun: testRun.Name},
	); err != nil {
		return nil, err
	}
	return podList.Items, nil
}

// deleteOwnedPods deletes all pods owned by the given TestRun.
func (r *TestRunReconciler) deleteOwnedPods(ctx context.Context, testRun *jmeterv1.TestRun) error {
	pods, err := r.listOwnedPods(ctx, testRun)
	if err != nil {
		return err
	}
	for i := range pods {
		if err := r.Delete(ctx, &pods[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting pod %s: %w", pods[i].Name, err)
		}
	}
	return nil
}

// buildPod constructs a Pod object for the given run group.
// If ControllerConfig.PodTemplate is set it is used as the base; the controller
// then enforces labels, restartPolicy, image, nodeSelector and the three required
// env vars (TESTRUN_NAME / RUN_GROUP / THREAD_COUNT).
func (r *TestRunReconciler) buildPod(
	testRun *jmeterv1.TestRun,
	groupName, name string,
	threadCount int32,
	nodeSelector map[string]string,
) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testRun.Namespace,
		},
	}

	// Apply controller-level pod template as base
	if r.Config != nil && r.Config.PodTemplate != nil {
		tpl := r.Config.PodTemplate.DeepCopy()
		// Copy annotations and extra labels from template
		pod.Annotations = tpl.Annotations
		for k, v := range tpl.Labels {
			if pod.Labels == nil {
				pod.Labels = make(map[string]string)
			}
			pod.Labels[k] = v
		}
		pod.Spec = tpl.Spec
	}

	// Always enforce controller labels (may overwrite template labels with same key)
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels[labelTestRun] = testRun.Name
	pod.Labels[labelRunGroup] = groupName

	// Always enforce restartPolicy
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever

	// Merge nodeSelector: RunGroup selector takes precedence over template
	if len(nodeSelector) > 0 {
		if pod.Spec.NodeSelector == nil {
			pod.Spec.NodeSelector = make(map[string]string)
		}
		for k, v := range nodeSelector {
			pod.Spec.NodeSelector[k] = v
		}
	}

	// Find or create the "jmeter-slave" container
	slaveIdx := -1
	for i, c := range pod.Spec.Containers {
		if c.Name == "jmeter-slave" {
			slaveIdx = i
			break
		}
	}
	if slaveIdx == -1 {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "jmeter-slave"})
		slaveIdx = len(pod.Spec.Containers) - 1
	}

	// Enforce image and required env vars on the jmeter-slave container
	pod.Spec.Containers[slaveIdx].Image = testRun.Spec.SlaveImage
	pod.Spec.Containers[slaveIdx].Env = mergeEnvVars(
		pod.Spec.Containers[slaveIdx].Env,
		[]corev1.EnvVar{
			{Name: "TESTRUN_NAME", Value: testRun.Name},
			{Name: "RUN_GROUP", Value: groupName},
			{Name: "THREAD_COUNT", Value: strconv.Itoa(int(threadCount))},
		},
	)

	// Set ownerReference so pods are GC-ed when TestRun is deleted
	_ = controllerutil.SetControllerReference(testRun, pod, r.Scheme)
	return pod
}

// mergeEnvVars returns base env vars with overrides applied (override wins on duplicate Name).
func mergeEnvVars(base, overrides []corev1.EnvVar) []corev1.EnvVar {
	result := make([]corev1.EnvVar, 0, len(base)+len(overrides))
	seen := make(map[string]int)
	for _, e := range base {
		seen[e.Name] = len(result)
		result = append(result, e)
	}
	for _, e := range overrides {
		if idx, exists := seen[e.Name]; exists {
			result[idx] = e
		} else {
			result = append(result, e)
		}
	}
	return result
}

// podName returns a deterministic pod name.
func podName(testRunName, groupName string, index int) string {
	return fmt.Sprintf("%s-%s-%d", testRunName, groupName, index)
}

// podThreadCount reads the THREAD_COUNT env var from a pod.
func podThreadCount(pod *corev1.Pod) int32 {
	for _, c := range pod.Spec.Containers {
		for _, env := range c.Env {
			if env.Name == "THREAD_COUNT" {
				v, _ := strconv.ParseInt(env.Value, 10, 32)
				return int32(v)
			}
		}
	}
	return 0
}

// SetupWithManager sets up the controller with the Manager.
func (r *TestRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&jmeterv1.TestRun{}).
		// Watch pods owned by a TestRun and map back to the TestRun for reconcile
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestForOwner(mgr.GetScheme(), mgr.GetRESTMapper(), &jmeterv1.TestRun{}, handler.OnlyControllerOwner()),
		).
		Complete(r)
}
