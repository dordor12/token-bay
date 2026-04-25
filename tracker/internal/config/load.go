package config

import (
	"errors"
	"io"
	"os"

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

// Load reads the YAML config file at path, applies defaults, and validates.
// Returns a typed *ParseError on parse failure or *ValidationError on
// invariant failure (both errors.As-comparable).
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	c, err := Parse(f)
	if err != nil {
		var pe *ParseError
		if errors.As(err, &pe) {
			pe.Path = path
		}
		return nil, err
	}
	ApplyDefaults(c)
	if err := Validate(c); err != nil {
		return nil, err
	}
	return c, nil
}
