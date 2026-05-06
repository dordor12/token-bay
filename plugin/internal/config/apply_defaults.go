package config

import (
	"os"
	"path/filepath"
	"strings"
)

// ApplyDefaults fills every zero-valued field with the value from
// DefaultConfig and expands "~"-prefixed paths to the user's home
// directory. Idempotent: running it twice produces the same Config.
//
// Implementation policy: explicit per-field code, no reflection.
// Future contributors who add fields must add a corresponding
// default-fill stanza here.
func ApplyDefaults(c *Config) {
	d := DefaultConfig()

	if c.IdentityKeyPath == "" {
		c.IdentityKeyPath = d.IdentityKeyPath
	}
	if c.AuditLogPath == "" {
		c.AuditLogPath = d.AuditLogPath
	}
	c.IdentityKeyPath = expandTilde(c.IdentityKeyPath)
	c.AuditLogPath = expandTilde(c.AuditLogPath)

	// CC bridge
	if c.CCBridge.ClaudeBin == "" {
		c.CCBridge.ClaudeBin = d.CCBridge.ClaudeBin
	}
	if c.CCBridge.ExtraFlags == nil {
		c.CCBridge.ExtraFlags = d.CCBridge.ExtraFlags
	}
	// Sandbox.Enabled is a bool — false is its valid default; nothing to fill.
	if c.CCBridge.Sandbox.Driver == "" {
		c.CCBridge.Sandbox.Driver = d.CCBridge.Sandbox.Driver
	}

	// Consumer / seeder
	if c.Consumer.NetworkModeTTL == 0 {
		c.Consumer.NetworkModeTTL = d.Consumer.NetworkModeTTL
	}
	if c.Seeder.HeadroomWindow == 0 {
		c.Seeder.HeadroomWindow = d.Seeder.HeadroomWindow
	}

	// Idle policy
	if c.IdlePolicy.Mode == "" {
		c.IdlePolicy.Mode = d.IdlePolicy.Mode
	}
	if c.IdlePolicy.Window == "" {
		c.IdlePolicy.Window = d.IdlePolicy.Window
	}
	if c.IdlePolicy.ActivityGrace == 0 {
		c.IdlePolicy.ActivityGrace = d.IdlePolicy.ActivityGrace
	}

	// Top-level scalars
	if c.PrivacyTier == "" {
		c.PrivacyTier = d.PrivacyTier
	}
	if c.MaxSpendPerHour == 0 {
		c.MaxSpendPerHour = d.MaxSpendPerHour
	}
}

// expandTilde replaces a leading "~" or "~/..." with the user's home
// directory. Returns the input unchanged on any failure (Validate
// flags non-absolute paths separately).
func expandTilde(p string) string {
	if p == "" {
		return p
	}
	if p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return home
	}
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return filepath.Join(home, p[2:])
	}
	return p
}
