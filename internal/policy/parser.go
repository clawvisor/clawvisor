package policy

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Parse parses a policy from YAML bytes.
func Parse(data []byte) (*Policy, error) {
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("policy: YAML parse error: %w", err)
	}
	return &p, nil
}

// ParseFile reads and parses a policy YAML file.
func ParseFile(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("policy: reading file %q: %w", path, err)
	}
	return Parse(data)
}
