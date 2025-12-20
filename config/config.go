package config

import (
	"errors"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

const (
	// MaxSensibleQueueSizeBytes is the maximum reasonable queue size to catch configuration typos
	// Set to 1GB (1024 * 1024 * 1024 bytes) as an upper bound
	MaxSensibleQueueSizeBytes = 1024 * 1024 * 1024
)

func (c Config) Validate() error {
	if c.DataDirectory == "" {
		return errors.New("invalid data directory")
	}

	return nil
}

// ReadConfig parses data and returns Config.
// Error during parsing or an invalid configuration in the Config will return an error.
func ReadConfig(data []byte) (*Config, error) {
	// not using a default config to detect badger ds users
	config := Config{}

	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, config.Validate()
}

func (c Config) Export() ([]byte, error) {
	sb := strings.Builder{}
	sb.WriteString("######################\n")
	sb.WriteString("### cygnus Config ###\n")
	sb.WriteString("######################\n\n")

	d, err := yaml.Marshal(&c)
	if err != nil {
		return nil, err
	}

	sb.Write(d)

	sb.WriteString("\n######################\n")

	return []byte(sb.String()), nil
}
