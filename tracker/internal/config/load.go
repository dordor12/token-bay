package config

import (
	"io"

	"gopkg.in/yaml.v3"
)

// Parse decodes a YAML document from r into a *Config. Unknown fields are
// rejected — typos must fail loudly. Parse does not apply defaults or
// validate; for the full pipeline use Load.
func Parse(r io.Reader) (*Config, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)

	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, &ParseError{Err: err}
	}
	return &c, nil
}
