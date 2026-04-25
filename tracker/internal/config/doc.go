// Package config loads and validates the token-bay-tracker YAML
// configuration file.
//
// The package is a leaf: it depends only on the standard library and
// gopkg.in/yaml.v3. Every other tracker subsystem reads its section
// from the *Config returned by Load.
//
// Public surface:
//
//	Load(path)         file → *Config | error      (parse + defaults + validate)
//	Parse(io.Reader)   YAML stream → *Config | error  (no defaults, no validate)
//	ApplyDefaults(*Config)
//	Validate(*Config)  → error (typed *ValidationError)
//	DefaultConfig()    *Config with every defaultable field populated
//
// See docs/superpowers/specs/tracker/2026-04-25-tracker-internal-config-design.md
// for the full schema and validation rules. See CLAUDE.md in this
// directory for instructions on extending the schema.
package config
