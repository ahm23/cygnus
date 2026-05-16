package config

import (
	"errors"
	"strings"

	yaml "gopkg.in/yaml.v3"
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
	config := Config{}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	return &config, config.Validate()
}

func (c Config) Export() ([]byte, error) {
	sb := strings.Builder{}
	sb.WriteString("=======================\n")
	sb.WriteString("#### Cygnus Config ####\n")
	sb.WriteString("=======================\n\n")

	d, err := yaml.Marshal(&c)
	if err != nil {
		return nil, err
	}

	sb.Write(d)
	sb.WriteString("\n=======================\n")

	return []byte(sb.String()), nil
}
