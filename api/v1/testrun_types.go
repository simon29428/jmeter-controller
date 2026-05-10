package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MountSpec defines a volume (ConfigMap or PVC) to be mounted into all pods of a TestRun.
type MountSpec struct {
	// Name is the unique name for this volume, used as the Kubernetes volume name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// MountPath is the absolute path in the container where the volume is mounted.
	// +kubebuilder:validation:MinLength=1
	MountPath string `json:"mountPath"`

	// ConfigMap is the name of the ConfigMap to mount.
	// Mutually exclusive with ClaimName.
	// +optional
	ConfigMap string `json:"configMap,omitempty"`

	// ClaimName is the name of the PersistentVolumeClaim to mount.
	// Mutually exclusive with ConfigMap.
	// +optional
	ClaimName string `json:"claimName,omitempty"`
}

// SlaveSpec defines the configuration for JMeter worker (slave) pods.
type SlaveSpec struct {
	// Image is the container image used for JMeter slave pods.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Env defines additional environment variables injected into each slave container.
	// Values support Kubernetes $(VAR_NAME) substitution referencing other env vars
	// defined earlier in the list.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Mounts defines additional volumes (ConfigMap or PVC) to mount into slave pods.
	// +optional
	Mounts []MountSpec `json:"mounts,omitempty"`
}

// MasterSpec defines the configuration for the JMeter master pod.
type MasterSpec struct {
	// Image is the container image used for the JMeter master pod.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// ScriptPath is the path to the JMeter test script (.jmx) to execute.
	// The value is passed to the master container as the SCRIPT_PATH environment variable.
	// Supports Kubernetes $(VAR_NAME) substitution referencing env vars defined in Env.
	// +kubebuilder:validation:MinLength=1
	ScriptPath string `json:"scriptPath"`

	// ReportPath is the output directory for the JMeter test report.
	// The value is passed to the master container as the REPORT_PATH environment variable.
	// Supports Kubernetes $(VAR_NAME) substitution referencing env vars defined in Env.
	// +optional
	ReportPath string `json:"reportPath,omitempty"`

	// Env defines additional environment variables injected into the master container.
	// Values support Kubernetes $(VAR_NAME) substitution referencing other env vars
	// defined earlier in the list. These are injected before SCRIPT_PATH and REPORT_PATH,
	// so they can be referenced by those fields via $(VAR_NAME) syntax.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Mounts defines additional volumes (ConfigMap or PVC) to mount into the master pod.
	// +optional
	Mounts []MountSpec `json:"mounts,omitempty"`
}

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
	// Slave defines the configuration for JMeter worker (slave) pods.
	Slave SlaveSpec `json:"slave"`

	// Master defines the configuration for the JMeter master pod.
	// When set, the controller waits for all worker pods to become Ready, then
	// creates a single master pod with SLAVE_HOSTS set to the comma-separated
	// list of worker IP addresses. If nil, no master pod is created.
	// +optional
	Master *MasterSpec `json:"master,omitempty"`

	// RunGroups is a map of run group name to its configuration
	// +kubebuilder:validation:MinProperties=1
	RunGroups map[string]RunGroupConfig `json:"runGroups"`
}

// TestRunPhase represents the lifecycle phase of a TestRun
type TestRunPhase string

const (
	TestRunPhasePending      TestRunPhase = "Pending"
	TestRunPhaseWaiting      TestRunPhase = "Waiting"
	TestRunPhaseWorkersReady TestRunPhase = "WorkersReady"
	TestRunPhaseRunning      TestRunPhase = "Running"
	TestRunPhaseCompleted    TestRunPhase = "Completed"
	TestRunPhaseFailed       TestRunPhase = "Failed"
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
	// +kubebuilder:validation:Enum=Pending;Waiting;WorkersReady;Running;Completed;Failed
	// +optional
	Phase TestRunPhase `json:"phase,omitempty"`

	// Message is a human-readable message indicating details about the current phase
	// +optional
	Message string `json:"message,omitempty"`

	// StartTime is the time when the TestRun transitioned into Running phase
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// Pods contains information about all worker pods managed by this TestRun
	// +optional
	Pods []PodInfo `json:"pods,omitempty"`

	// MasterPod contains information about the master pod, if one has been created
	// +optional
	MasterPod *PodInfo `json:"masterPod,omitempty"`
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
