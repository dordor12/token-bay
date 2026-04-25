package config

import (
	"net"
	"path/filepath"
)

// Validate runs every cross-field invariant declared in the design spec
// §6 against c and returns a *ValidationError with every failure
// recorded. Returns nil when c is valid.
//
// Order: rules run in section order so error output is stable for tests.
// Validation never short-circuits on first error — operators must see
// every problem in one run.
func Validate(c *Config) error {
	v := &validator{}
	v.checkRequired(c)
	v.checkLogLevel(c)
	v.checkListeners(c)
	return v.toError()
}

type validator struct {
	errs []FieldError
}

func (v *validator) add(field, msg string) {
	v.errs = append(v.errs, FieldError{Field: field, Message: msg})
}

func (v *validator) toError() error {
	if len(v.errs) == 0 {
		return nil
	}
	return &ValidationError{Errors: v.errs}
}

// §6.1
func (v *validator) checkRequired(c *Config) {
	if c.DataDir == "" {
		v.add("data_dir", "must be non-empty")
	} else if !filepath.IsAbs(c.DataDir) {
		v.add("data_dir", "must be an absolute path")
	}
	if c.Server.ListenAddr == "" {
		v.add("server.listen_addr", "must be non-empty")
	}
	if c.Server.IdentityKeyPath == "" {
		v.add("server.identity_key_path", "must be non-empty")
	}
	if c.Server.TLSCertPath == "" {
		v.add("server.tls_cert_path", "must be non-empty")
	}
	if c.Server.TLSKeyPath == "" {
		v.add("server.tls_key_path", "must be non-empty")
	}
	if c.Ledger.StoragePath == "" {
		v.add("ledger.storage_path", "must be non-empty")
	}
}

// §6.1 — log level enum
func (v *validator) checkLogLevel(c *Config) {
	switch c.LogLevel {
	case "", "debug", "info", "warn", "error":
		// "" is allowed because ApplyDefaults fills it; if Validate is
		// called without ApplyDefaults the empty value is still valid.
	default:
		v.add("log_level", "must be one of: debug, info, warn, error")
	}
}

// §6.2 — every listener parses; all five collectively distinct.
func (v *validator) checkListeners(c *Config) {
	listeners := []struct {
		field string
		addr  string
	}{
		{"server.listen_addr", c.Server.ListenAddr},
		{"admin.listen_addr", c.Admin.ListenAddr},
		{"metrics.listen_addr", c.Metrics.ListenAddr},
		{"stun_turn.stun_listen_addr", c.STUNTURN.STUNListenAddr},
		{"stun_turn.turn_listen_addr", c.STUNTURN.TURNListenAddr},
	}
	seen := make(map[string]string, len(listeners))
	for _, l := range listeners {
		if l.addr == "" {
			// Required listeners are caught by checkRequired; optional
			// listeners with empty addr are not parsed.
			continue
		}
		if _, _, err := net.SplitHostPort(l.addr); err != nil {
			v.add(l.field, "must be a valid host:port — "+err.Error())
			continue
		}
		if prev, ok := seen[l.addr]; ok {
			v.add(l.field, "collides with "+prev+" (both "+l.addr+")")
			continue
		}
		seen[l.addr] = l.field
	}
}
