package config

import (
	"fmt"
	"time"

	"github.com/goccy/go-yaml"
)

// Duration uses Go duration strings in YAML and rejects zero or negative
// values. It deliberately does not accept numeric duration units.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(data []byte) error {
	var raw string
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("duration must be a string")
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration")
	}
	if parsed <= 0 {
		return fmt.Errorf("duration must be greater than zero")
	}
	d.Duration = parsed
	return nil
}
