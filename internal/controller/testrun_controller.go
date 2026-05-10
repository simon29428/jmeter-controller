package controller

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	jmeterv1 "jmeter-controller/api/v1"
	"jmeter-controller/internal/config"
)

const (
	finalizerName         = "jmeter.io/finalizer"
	labelTestRun          = "jmeter.io/testrun"
	labelRunGroup         = "jmeter.io/rungroup"
	labelRole             = "jmeter.io/role"
	labelRoleWorker       = "worker"
	labelRoleMaster       = "master"
	defaultBase     int32 = 50
)

// TestRunReconciler reconciles a TestRun object
type TestRunReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Config *config.ControllerConfig
}

// +kubebuilder:rbac:groups=jmeter.io,resources=testruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=jmeter.io,resources=testruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=jmeter.io,resources=testruns/finalizers,verbs=update
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
			return ctrl.Result{}, client.IgnoreNotFound(err)
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

	// Ensure all worker pods exist
	if err := r.reconcilePods(ctx, testRun); err != nil {
		return ctrl.Result{}, err
	}

	// Fetch current worker pods to determine if master should be started
	workerPods, err := r.listOwnedWorkerPods(ctx, testRun)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Once all workers are ready, create the master pod if configured
	if err := r.reconcileMasterPod(ctx, testRun, workerPods); err != nil {
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
		return ctrl.Result{}, client.IgnoreNotFound(err)
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

	// Count active (Running or Pending) TestRuns per run group (excluding self).
	// Pending is included because it means the TestRun has already passed the limit
	// check and is creating pods — it occupies a concurrent slot.
	activePerGroup := make(map[string]int32)
	for i := range allRuns.Items {
		other := &allRuns.Items[i]
		if other.UID == testRun.UID {
			continue
		}
		phase := other.Status.Phase
		if phase != jmeterv1.TestRunPhaseRunning && phase != jmeterv1.TestRunPhasePending && phase != jmeterv1.TestRunPhaseWorkersReady {
			continue
		}
		for groupName := range other.Spec.RunGroups {
			activePerGroup[groupName]++
		}
	}

	for groupName := range testRun.Spec.RunGroups {
		limit := r.Config.MaxConcurrentForGroup(groupName)
		if limit > 0 && activePerGroup[groupName] >= limit {
			return true, fmt.Sprintf("run group %q has reached its concurrent limit of %d", groupName, limit), nil
		}
	}
	return false, "", nil
}

// reconcilePods ensures that the desired pods exist for every run group.
func (r *TestRunReconciler) reconcilePods(ctx context.Context, testRun *jmeterv1.TestRun) error {
	logger := log.FromContext(ctx)

	existingPods, err := r.listOwnedWorkerPods(ctx, testRun)
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
	workerPods, err := r.listOwnedWorkerPods(ctx, testRun)
	if err != nil {
		return ctrl.Result{}, err
	}

	masterPod, err := r.listOwnedMasterPod(ctx, testRun)
	if err != nil {
		return ctrl.Result{}, err
	}

	podInfos := make([]jmeterv1.PodInfo, 0, len(workerPods))
	for _, pod := range workerPods {
		podInfos = append(podInfos, jmeterv1.PodInfo{
			Name:        pod.Name,
			IP:          pod.Status.PodIP,
			RunGroup:    pod.Labels[labelRunGroup],
			ThreadCount: podThreadCount(&pod),
			Phase:       pod.Status.Phase,
		})
	}

	var masterPodInfo *jmeterv1.PodInfo
	if masterPod != nil {
		masterPodInfo = &jmeterv1.PodInfo{
			Name:  masterPod.Name,
			IP:    masterPod.Status.PodIP,
			Phase: masterPod.Status.Phase,
		}
	}

	phase, message := computePhase(testRun, workerPods, masterPod)
	patch := client.MergeFrom(testRun.DeepCopy())
	testRun.Status.Pods = podInfos
	testRun.Status.MasterPod = masterPodInfo
	testRun.Status.Phase = phase
	testRun.Status.Message = message
	if phase == jmeterv1.TestRunPhaseRunning && testRun.Status.StartTime == nil {
		now := metav1.Now()
		testRun.Status.StartTime = &now
	}

	if err := r.Status().Patch(ctx, testRun, patch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

// computePhase determines the overall TestRun phase from the owned pods.
// masterPod may be nil if not yet created or if spec.master is not set.
func computePhase(testRun *jmeterv1.TestRun, workerPods []corev1.Pod, masterPod *corev1.Pod) (jmeterv1.TestRunPhase, string) {
	// Calculate expected total worker pod count
	expectedCount := 0
	for _, g := range testRun.Spec.RunGroups {
		base := g.Base
		if base <= 0 {
			base = defaultBase
		}
		expectedCount += int(math.Ceil(float64(g.Thread) / float64(base)))
	}

	if len(workerPods) == 0 {
		return jmeterv1.TestRunPhasePending, "Waiting for worker pods to start"
	}

	// When no master is configured, use the original worker-only logic.
	if testRun.Spec.Master == nil {
		allDone := true
		anyFailed := false
		for _, pod := range workerPods {
			switch pod.Status.Phase {
			case corev1.PodSucceeded:
				// done
			case corev1.PodFailed:
				anyFailed = true
			default:
				allDone = false
			}
		}
		if allDone && len(workerPods) >= expectedCount {
			if anyFailed {
				return jmeterv1.TestRunPhaseFailed, "One or more pods failed"
			}
			return jmeterv1.TestRunPhaseCompleted, "All pods completed successfully"
		}
		return jmeterv1.TestRunPhaseRunning, fmt.Sprintf("%d/%d pods running", len(workerPods), expectedCount)
	}

	// Master-enabled mode: wait for all workers to be Ready before starting master.
	readyCount := 0
	for i := range workerPods {
		if isPodReady(&workerPods[i]) {
			readyCount++
		}
	}
	if readyCount < expectedCount {
		return jmeterv1.TestRunPhasePending, fmt.Sprintf("%d/%d workers ready", readyCount, expectedCount)
	}

	// All workers are ready; check master state.
	if masterPod == nil {
		return jmeterv1.TestRunPhaseWorkersReady, "All workers ready, waiting for master pod to start"
	}

	switch masterPod.Status.Phase {
	case corev1.PodRunning:
		return jmeterv1.TestRunPhaseRunning, "Test running"
	case corev1.PodSucceeded:
		// Master finished; check workers too.
		anyFailed := false
		allWorkersDone := true
		for _, pod := range workerPods {
			switch pod.Status.Phase {
			case corev1.PodSucceeded:
				// ok
			case corev1.PodFailed:
				anyFailed = true
			default:
				allWorkersDone = false
			}
		}
		if !allWorkersDone {
			return jmeterv1.TestRunPhaseRunning, "Master completed, waiting for workers to finish"
		}
		if anyFailed {
			return jmeterv1.TestRunPhaseFailed, "One or more pods failed"
		}
		return jmeterv1.TestRunPhaseCompleted, "All pods completed successfully"
	case corev1.PodFailed:
		return jmeterv1.TestRunPhaseFailed, "Master pod failed"
	default:
		return jmeterv1.TestRunPhaseWorkersReady, "Master pod starting"
	}
}

// setPhase patches only the Status.Phase and Status.Message fields.
func (r *TestRunReconciler) setPhase(ctx context.Context, testRun *jmeterv1.TestRun, phase jmeterv1.TestRunPhase, message string, result ctrl.Result) (ctrl.Result, error) {
	patch := client.MergeFrom(testRun.DeepCopy())
	testRun.Status.Phase = phase
	testRun.Status.Message = message
	if err := r.Status().Patch(ctx, testRun, patch); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return result, nil
}

// listOwnedWorkerPods returns all worker pods owned by this TestRun.
func (r *TestRunReconciler) listOwnedWorkerPods(ctx context.Context, testRun *jmeterv1.TestRun) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(testRun.Namespace),
		client.MatchingLabels{labelTestRun: testRun.Name, labelRole: labelRoleWorker},
	); err != nil {
		return nil, err
	}
	return podList.Items, nil
}

// listOwnedMasterPod returns the master pod owned by this TestRun, or nil if it does not exist.
func (r *TestRunReconciler) listOwnedMasterPod(ctx context.Context, testRun *jmeterv1.TestRun) (*corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(testRun.Namespace),
		client.MatchingLabels{labelTestRun: testRun.Name, labelRole: labelRoleMaster},
	); err != nil {
		return nil, err
	}
	if len(podList.Items) == 0 {
		return nil, nil
	}
	return &podList.Items[0], nil
}

// deleteOwnedPods deletes all pods (workers and master) owned by the given TestRun.
func (r *TestRunReconciler) deleteOwnedPods(ctx context.Context, testRun *jmeterv1.TestRun) error {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(testRun.Namespace),
		client.MatchingLabels{labelTestRun: testRun.Name},
	); err != nil {
		return err
	}
	for i := range podList.Items {
		if err := r.Delete(ctx, &podList.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting pod %s: %w", podList.Items[i].Name, err)
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
	pod.Labels[labelRole] = labelRoleWorker

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
	pod.Spec.Containers[slaveIdx].Image = testRun.Spec.Slave.Image
	pod.Spec.Containers[slaveIdx].Env = mergeEnvVars(
		pod.Spec.Containers[slaveIdx].Env,
		testRun.Spec.Slave.Env,
	)
	pod.Spec.Containers[slaveIdx].Env = mergeEnvVars(
		pod.Spec.Containers[slaveIdx].Env,
		[]corev1.EnvVar{
			{Name: "TESTRUN_NAME", Value: testRun.Name},
			{Name: "RUN_GROUP", Value: groupName},
			{Name: groupName + "_THREAD_COUNT", Value: strconv.Itoa(int(threadCount))},
		},
	)
	// Apply TestRun-level mounts to the worker container.
	applyMounts(testRun.Spec.Slave.Mounts, pod, "jmeter-slave")
	// Set ownerReference so pods are GC-ed when TestRun is deleted
	_ = controllerutil.SetControllerReference(testRun, pod, r.Scheme)
	return pod
}

// applyMounts adds volumes and volumeMounts from the given mounts list to the pod.
// It targets the container identified by containerName and skips entries where
// neither ConfigMap nor PVC is set. Duplicates (by volume name) are not added twice.
func applyMounts(mounts []jmeterv1.MountSpec, pod *corev1.Pod, containerName string) {
	for _, m := range mounts {
		// Build the Volume source.
		var volSrc corev1.VolumeSource
		switch {
		case m.ConfigMap != "":
			volSrc = corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: m.ConfigMap},
					DefaultMode:          func() *int32 { i := int32(0755); return &i }(),
				},
			}
		case m.PVC != "":
			volSrc = corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: m.PVC,
				},
			}
		default:
			continue // neither source set — skip
		}

		// Add Volume if not already present (template may have declared it).
		volFound := false
		for _, v := range pod.Spec.Volumes {
			if v.Name == m.Name {
				volFound = true
				break
			}
		}
		if !volFound {
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: m.Name, VolumeSource: volSrc})
		}

		// Add VolumeMount to the target container if not already present.
		for i, c := range pod.Spec.Containers {
			if c.Name != containerName {
				continue
			}
			mountFound := false
			for _, vm := range c.VolumeMounts {
				if vm.Name == m.Name {
					mountFound = true
					break
				}
			}
			if !mountFound {
				pod.Spec.Containers[i].VolumeMounts = append(
					pod.Spec.Containers[i].VolumeMounts,
					corev1.VolumeMount{Name: m.Name, MountPath: m.MountPath},
				)
			}
			break
		}
	}
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

// podName returns a deterministic worker pod name.
func podName(testRunName, groupName string, index int) string {
	return fmt.Sprintf("%s-%s-%d", testRunName, groupName, index)
}

// masterPodName returns the deterministic name for the master pod of a TestRun.
func masterPodName(testRunName string) string {
	return fmt.Sprintf("%s-master", testRunName)
}

// isPodReady returns true if the pod's Ready condition is True.
func isPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

// reconcileMasterPod creates the master pod once all worker pods are Ready and have IPs.
// It is a no-op when spec.master is not set or the master pod already exists.
func (r *TestRunReconciler) reconcileMasterPod(ctx context.Context, testRun *jmeterv1.TestRun, workerPods []corev1.Pod) error {
	if testRun.Spec.Master == nil {
		return nil
	}

	// Check if master already exists.
	existing, err := r.listOwnedMasterPod(ctx, testRun)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil
	}

	// Calculate expected worker count.
	expectedCount := 0
	for _, g := range testRun.Spec.RunGroups {
		base := g.Base
		if base <= 0 {
			base = defaultBase
		}
		expectedCount += int(math.Ceil(float64(g.Thread) / float64(base)))
	}

	// Wait until all workers are Ready and have IPs.
	if len(workerPods) < expectedCount {
		return nil
	}
	slaveHosts := make([]string, 0, len(workerPods))
	for i := range workerPods {
		if !isPodReady(&workerPods[i]) || workerPods[i].Status.PodIP == "" {
			return nil
		}
		slaveHosts = append(slaveHosts, workerPods[i].Status.PodIP)
	}

	logger := log.FromContext(ctx)
	name := masterPodName(testRun.Name)
	pod := r.buildMasterPod(testRun, name, strings.Join(slaveHosts, ","))
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating master pod %s: %w", name, err)
	}
	logger.Info("Created master pod", "pod", name, "slaveHosts", strings.Join(slaveHosts, ","))
	return nil
}

// buildMasterPod constructs the master Pod object.
// If ControllerConfig.MasterPodTemplate is set it is used as the base; the controller
// then enforces labels, restartPolicy, image, and TESTRUN_NAME/SLAVE_HOSTS env vars.
func (r *TestRunReconciler) buildMasterPod(testRun *jmeterv1.TestRun, name, slaveHosts string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testRun.Namespace,
		},
	}

	// Apply controller-level master pod template as base.
	if r.Config != nil && r.Config.MasterPodTemplate != nil {
		tpl := r.Config.MasterPodTemplate.DeepCopy()
		pod.Annotations = tpl.Annotations
		for k, v := range tpl.Labels {
			if pod.Labels == nil {
				pod.Labels = make(map[string]string)
			}
			pod.Labels[k] = v
		}
		pod.Spec = tpl.Spec
	}

	// Always enforce controller labels.
	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	pod.Labels[labelTestRun] = testRun.Name
	pod.Labels[labelRole] = labelRoleMaster

	// Always enforce restartPolicy.
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever

	// Find or create the "jmeter-master" container.
	masterIdx := -1
	for i, c := range pod.Spec.Containers {
		if c.Name == "jmeter-master" {
			masterIdx = i
			break
		}
	}
	if masterIdx == -1 {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "jmeter-master"})
		masterIdx = len(pod.Spec.Containers) - 1
	}

	// Enforce image and required env vars on the jmeter-master container.
	pod.Spec.Containers[masterIdx].Image = testRun.Spec.Master.Image
	pod.Spec.Containers[masterIdx].Env = mergeEnvVars(
		pod.Spec.Containers[masterIdx].Env,
		testRun.Spec.Master.Env,
	)
	controllerEnvVars := []corev1.EnvVar{
		{Name: "TESTRUN_NAME", Value: testRun.Name},
		{Name: "SLAVE_HOSTS", Value: slaveHosts},
		{Name: "SCRIPT_PATH", Value: testRun.Spec.Master.ScriptPath},
	}
	if testRun.Spec.Master.ReportPath != "" {
		controllerEnvVars = append(controllerEnvVars, corev1.EnvVar{Name: "REPORT_PATH", Value: testRun.Spec.Master.ReportPath})
	}
	pod.Spec.Containers[masterIdx].Env = mergeEnvVars(
		pod.Spec.Containers[masterIdx].Env,
		controllerEnvVars,
	)

	// Apply TestRun-level mounts to the master container.
	applyMounts(testRun.Spec.Master.Mounts, pod, "jmeter-master")

	// Set ownerReference so the pod is GC-ed when TestRun is deleted.
	_ = controllerutil.SetControllerReference(testRun, pod, r.Scheme)
	return pod
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
		// Watch all TestRuns: when one reaches a terminal phase, immediately
		// re-enqueue all Waiting TestRuns in the same namespace.
		Watches(
			&jmeterv1.TestRun{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueWaitingTestRuns),
		).
		Complete(r)
}

// enqueueWaitingTestRuns is invoked whenever any TestRun changes.
// When a TestRun reaches Completed or Failed it frees up a concurrent slot, so
// we immediately re-enqueue all Waiting TestRuns in the same namespace.
func (r *TestRunReconciler) enqueueWaitingTestRuns(ctx context.Context, obj client.Object) []reconcile.Request {
	testRun, ok := obj.(*jmeterv1.TestRun)
	if !ok {
		return nil
	}
	// Only act when a slot has been freed
	phase := testRun.Status.Phase
	if phase != jmeterv1.TestRunPhaseCompleted && phase != jmeterv1.TestRunPhaseFailed {
		return nil
	}

	allRuns := &jmeterv1.TestRunList{}
	if err := r.List(ctx, allRuns, client.InNamespace(testRun.Namespace)); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for _, tr := range allRuns.Items {
		if tr.UID == testRun.UID {
			continue
		}
		if tr.Status.Phase == jmeterv1.TestRunPhaseWaiting {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: tr.Namespace,
					Name:      tr.Name,
				},
			})
		}
	}
	return requests
}
