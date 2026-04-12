package repo

import (
	"os"

	"gopkg.in/yaml.v3"

	"github.com/aisu-ai/aidev/internal/config"
)

// LoadRepoPrinciples reads a principles.yaml co-located with a target repo
// (e.g. .aidev/principles.yaml) and returns its Principle slice. It is
// tolerant: a missing file returns (nil, nil) because not every repo ships
// its own principles — aidev falls back to the global config file.
func LoadRepoPrinciples(path string) ([]config.Principle, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var p config.Principles
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return p.Principles, nil
}
