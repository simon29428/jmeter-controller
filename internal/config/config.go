package config

import (
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// ControllerConfig holds the runtime configuration for the jmeter controller
type ControllerConfig struct {
	// RunGroupLimits defines the maximum number of concurrently running TestRuns
	// per run group name. For example:
	//   runGroupLimits:
	//     group-a: 2
	//     group-b: 1
	RunGroupLimits map[string]int32 `json:"runGroupLimits,omitempty"`

	// PodTemplate is the base template applied to every slave pod created by the controller.
	// The controller always enforces:
	//   - metadata.labels: jmeter.jmeter.io/testrun and jmeter.jmeter.io/rungroup
	//   - spec.restartPolicy: Never
	//   - the "jmeter-slave" container image and TESTRUN_NAME/RUN_GROUP/THREAD_COUNT env vars
	// All other fields (resources, volumeMounts, volumes, tolerations, affinity, etc.)
	// are taken from this template as-is.
	PodTemplate *corev1.PodTemplateSpec `json:"podTemplate,omitempty"`

	// MasterPodTemplate is the base template applied to the master pod created by the controller.
	// The controller always enforces:
	//   - metadata.labels: jmeter.jmeter.io/testrun and jmeter.jmeter.io/role=master
	//   - spec.restartPolicy: Never
	//   - the "jmeter-master" container image and TESTRUN_NAME/SLAVE_HOSTS env vars
	// All other fields (resources, volumeMounts, volumes, tolerations, affinity, etc.)
	// are taken from this template as-is.
	MasterPodTemplate *corev1.PodTemplateSpec `json:"masterPodTemplate,omitempty"`
}

// MaxConcurrentForGroup returns the max concurrent TestRun limit for a given
// run group. Returns 0 (unlimited) if no explicit limit is configured for the group.
func (c *ControllerConfig) MaxConcurrentForGroup(groupName string) int32 {
	if c == nil || c.RunGroupLimits == nil {
		return 0
	}
	if limit, ok := c.RunGroupLimits[groupName]; ok && limit > 0 {
		return limit
	}
	return 0
}

// LoadConfig reads and parses the controller config YAML file at the given path.
// Returns an empty ControllerConfig (all defaults) if path is empty.
func LoadConfig(path string) (*ControllerConfig, error) {
	cfg := &ControllerConfig{}
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading controller config %q: %w", path, err)
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing controller config %q: %w", path, err)
	}

	return cfg, nil
}
