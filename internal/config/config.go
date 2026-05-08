package config

import (
	"fmt"
	"os"

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
}

// MaxConcurrentForGroup returns the max concurrent TestRun limit for a given
// run group. If no explicit limit is configured, defaults to 1.
func (c *ControllerConfig) MaxConcurrentForGroup(groupName string) int32 {
	if c == nil || c.RunGroupLimits == nil {
		return 1
	}
	if limit, ok := c.RunGroupLimits[groupName]; ok && limit > 0 {
		return limit
	}
	return 1
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
