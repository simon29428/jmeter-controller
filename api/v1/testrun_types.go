package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunGroupConfig defines the configuration for a single run group
type RunGroupConfig struct {
	// Thread is the total number of threads for this run group
	// +kubebuilder:validation:Minimum=1
	Thread int32 `json:"thread"`

	// Base is the number of threads per pod. Defaults to 50.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Base int32 `json:"base,omitempty"`

	// NodeSelector is an optional node selector applied to all pods in this run group
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

// TestRunSpec defines the desired state of TestRun
type TestRunSpec struct {
	// SlaveImage is the container image used for JMeter slave pods
	// +kubebuilder:validation:MinLength=1
	SlaveImage string `json:"slaveImage"`

	// RunGroups is a map of run group name to its configuration
	// +kubebuilder:validation:MinProperties=1
	RunGroups map[string]RunGroupConfig `json:"runGroups"`
}

// TestRunPhase represents the lifecycle phase of a TestRun
type TestRunPhase string

const (
	TestRunPhasePending   TestRunPhase = "Pending"
	TestRunPhaseWaiting   TestRunPhase = "Waiting"
	TestRunPhaseRunning   TestRunPhase = "Running"
	TestRunPhaseCompleted TestRunPhase = "Completed"
	TestRunPhaseFailed    TestRunPhase = "Failed"
)

// PodInfo holds information about a single slave pod
type PodInfo struct {
	// Name is the pod name
	Name string `json:"name"`

	// IP is the pod IP address
	// +optional
	IP string `json:"ip,omitempty"`

	// RunGroup is the run group this pod belongs to
	RunGroup string `json:"runGroup"`

	// ThreadCount is the number of threads assigned to this pod
	ThreadCount int32 `json:"threadCount"`

	// Phase is the current phase of the pod
	// +optional
	Phase corev1.PodPhase `json:"phase,omitempty"`
}

// TestRunStatus defines the observed state of TestRun
type TestRunStatus struct {
	// Phase is the current phase of the TestRun
	// +kubebuilder:validation:Enum=Pending;Waiting;Running;Completed;Failed
	// +optional
	Phase TestRunPhase `json:"phase,omitempty"`

	// Message is a human-readable message indicating details about the current phase
	// +optional
	Message string `json:"message,omitempty"`

	// StartTime is the time when the TestRun transitioned into Running phase
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// Pods contains information about all slave pods managed by this TestRun
	// +optional
	Pods []PodInfo `json:"pods,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=tr,singular=testrun
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Pods",type=integer,JSONPath=".status.pods[*].name",priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// TestRun is the Schema for the testruns API
type TestRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TestRunSpec   `json:"spec,omitempty"`
	Status TestRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TestRunList contains a list of TestRun
type TestRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TestRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TestRun{}, &TestRunList{})
}
