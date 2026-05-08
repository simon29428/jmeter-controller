package apiserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	jmeterv1 "jmeter-controller/api/v1"
)

const (
	labelTestRun  = "jmeter.jmeter.io/testrun"
	labelRunGroup = "jmeter.jmeter.io/rungroup"
)

// PodResponse is the JSON response body for a single pod entry
type PodResponse struct {
	Name        string          `json:"name"`
	IP          string          `json:"ip"`
	RunGroup    string          `json:"runGroup"`
	ThreadCount int32           `json:"threadCount"`
	Phase       corev1.PodPhase `json:"phase"`
}

// Server is the embedded HTTP API server
type Server struct {
	client client.Client
	port   int
	logger logr.Logger
}

// New creates a new API Server
func New(c client.Client, port int, logger logr.Logger) *Server {
	return &Server{client: c, port: port, logger: logger}
}

// Start registers routes and begins listening. Blocks until ctx is cancelled.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	// Cross-namespace: GET /api/v1/testruns
	mux.HandleFunc("/api/v1/testruns", s.listAllTestRuns)
	// Namespace-scoped routes
	mux.HandleFunc("/api/v1/namespaces/", s.routeHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", s.port),
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Shutdown when context is cancelled
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	s.logger.Info("API server listening", "port", s.port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// routeHandler dispatches requests to the correct handler based on the URL pattern:
//
//	GET  /api/v1/namespaces/{namespace}/testruns          → listTestRuns (in namespace)
//	GET  /api/v1/namespaces/{namespace}/testruns/{name}/pods  → listPods
//	POST /api/v1/namespaces/{namespace}/testruns/{name}/stop  → stopTestRun
func (s *Server) routeHandler(w http.ResponseWriter, r *http.Request) {
	// Try the longer pattern first: .../testruns/{name}/{action}
	namespace, name, action, err := parsePath(r.URL.Path)
	if err == nil {
		switch {
		case r.Method == http.MethodGet && action == "pods":
			s.listPods(w, r, namespace, name)
		case r.Method == http.MethodPost && action == "stop":
			s.stopTestRun(w, r, namespace, name)
		default:
			http.NotFound(w, r)
		}
		return
	}

	// Try shorter pattern: /api/v1/namespaces/{namespace}/testruns
	if ns, ok := parseNamespacedTestRunsPath(r.URL.Path); ok && r.Method == http.MethodGet {
		s.listTestRunsInNamespace(w, r, ns)
		return
	}

	http.NotFound(w, r)
}

// listPods returns the pods belonging to the specified TestRun.
func (s *Server) listPods(w http.ResponseWriter, r *http.Request, namespace, name string) {
	testRun := &jmeterv1.TestRun{}
	if err := s.client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, testRun); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, fmt.Sprintf("TestRun %s/%s not found", namespace, name), http.StatusNotFound)
			return
		}
		s.internalError(w, err)
		return
	}

	podList := &corev1.PodList{}
	if err := s.client.List(r.Context(), podList,
		client.InNamespace(namespace),
		client.MatchingLabels{labelTestRun: name},
	); err != nil {
		s.internalError(w, err)
		return
	}

	result := make([]PodResponse, 0, len(podList.Items))
	for _, pod := range podList.Items {
		result = append(result, PodResponse{
			Name:        pod.Name,
			IP:          pod.Status.PodIP,
			RunGroup:    pod.Labels[labelRunGroup],
			ThreadCount: podThreadCount(&pod),
			Phase:       pod.Status.Phase,
		})
	}

	writeJSON(w, http.StatusOK, result)
}

// stopTestRun marks the TestRun as Completed and deletes all owned pods.
func (s *Server) stopTestRun(w http.ResponseWriter, r *http.Request, namespace, name string) {
	testRun := &jmeterv1.TestRun{}
	if err := s.client.Get(r.Context(), client.ObjectKey{Namespace: namespace, Name: name}, testRun); err != nil {
		if apierrors.IsNotFound(err) {
			http.Error(w, fmt.Sprintf("TestRun %s/%s not found", namespace, name), http.StatusNotFound)
			return
		}
		s.internalError(w, err)
		return
	}

	// Patch status to Completed
	patch := client.MergeFrom(testRun.DeepCopy())
	testRun.Status.Phase = jmeterv1.TestRunPhaseCompleted
	testRun.Status.Message = "Stopped via REST API"
	if err := s.client.Status().Patch(r.Context(), testRun, patch); err != nil {
		s.internalError(w, err)
		return
	}

	// Delete all owned pods
	podList := &corev1.PodList{}
	if err := s.client.List(r.Context(), podList,
		client.InNamespace(namespace),
		client.MatchingLabels{labelTestRun: name},
	); err != nil {
		s.internalError(w, err)
		return
	}

	for i := range podList.Items {
		if err := s.client.Delete(r.Context(), &podList.Items[i]); err != nil && !apierrors.IsNotFound(err) {
			s.logger.Error(err, "failed to delete pod", "pod", podList.Items[i].Name)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "TestRun stopped"})
}

// TestRunResponse is the JSON representation of a single TestRun in list responses.
type TestRunResponse struct {
	Namespace string                `json:"namespace"`
	Name      string                `json:"name"`
	Phase     jmeterv1.TestRunPhase `json:"phase"`
	Message   string                `json:"message,omitempty"`
	StartTime string                `json:"startTime,omitempty"`
	Pods      []jmeterv1.PodInfo    `json:"pods"`
}

func toTestRunResponse(tr *jmeterv1.TestRun) TestRunResponse {
	startTime := ""
	if tr.Status.StartTime != nil {
		startTime = tr.Status.StartTime.UTC().Format("2006-01-02T15:04:05Z")
	}
	pods := tr.Status.Pods
	if pods == nil {
		pods = []jmeterv1.PodInfo{}
	}
	return TestRunResponse{
		Namespace: tr.Namespace,
		Name:      tr.Name,
		Phase:     tr.Status.Phase,
		Message:   tr.Status.Message,
		StartTime: startTime,
		Pods:      pods,
	}
}

// listAllTestRuns handles GET /api/v1/testruns — returns TestRuns across all namespaces.
func (s *Server) listAllTestRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	list := &jmeterv1.TestRunList{}
	if err := s.client.List(r.Context(), list); err != nil {
		s.internalError(w, err)
		return
	}
	result := make([]TestRunResponse, 0, len(list.Items))
	for i := range list.Items {
		result = append(result, toTestRunResponse(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, result)
}

// listTestRunsInNamespace handles GET /api/v1/namespaces/{namespace}/testruns.
func (s *Server) listTestRunsInNamespace(w http.ResponseWriter, r *http.Request, namespace string) {
	list := &jmeterv1.TestRunList{}
	if err := s.client.List(r.Context(), list, client.InNamespace(namespace)); err != nil {
		s.internalError(w, err)
		return
	}
	result := make([]TestRunResponse, 0, len(list.Items))
	for i := range list.Items {
		result = append(result, toTestRunResponse(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, result)
}

// parseNamespacedTestRunsPath parses /api/v1/namespaces/{namespace}/testruns
// and returns (namespace, true) on success.
func parseNamespacedTestRunsPath(path string) (string, bool) {
	// parts: ["", "api", "v1", "namespaces", namespace, "testruns"]
	parts := strings.Split(path, "/")
	if len(parts) == 6 && parts[1] == "api" && parts[2] == "v1" && parts[3] == "namespaces" && parts[5] == "testruns" && parts[4] != "" {
		return parts[4], true
	}
	return "", false
}

// parsePath extracts namespace, testrun name, and action from a URL path of the form:
// /api/v1/namespaces/{namespace}/testruns/{name}/{action}
func parsePath(path string) (namespace, name, action string, err error) {
	// strings.Split produces: ["", "api", "v1", "namespaces", ns, "testruns", name, action]
	parts := strings.Split(path, "/")
	if len(parts) != 8 || parts[1] != "api" || parts[2] != "v1" || parts[3] != "namespaces" || parts[5] != "testruns" {
		return "", "", "", fmt.Errorf("invalid path: %s", path)
	}
	return parts[4], parts[6], parts[7], nil
}

func podThreadCount(pod *corev1.Pod) int32 {
	for _, c := range pod.Spec.Containers {
		for _, env := range c.Env {
			if env.Name == "THREAD_COUNT" {
				v := int32(0)
				fmt.Sscanf(env.Value, "%d", &v)
				return v
			}
		}
	}
	return 0
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.logger.Error(err, "internal server error")
	http.Error(w, "internal server error", http.StatusInternalServerError)
}
