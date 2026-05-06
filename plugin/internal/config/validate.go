package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
)

// Validate runs every cross-field invariant declared in the design spec
// §2.4 against c and returns a *ValidationError with every failure
// recorded. Returns nil when c is valid.
//
// Order: rules run in section order so error output is stable for
// tests. Validation never short-circuits — operators must see every
// problem in one run.
func Validate(c *Config) error {
	v := &validator{}
	v.checkRequired(c)
	v.checkRole(c)
	v.checkTracker(c)
	v.checkPaths(c)
	v.checkCCBridge(c)
	v.checkConsumer(c)
	v.checkSeeder(c)
	v.checkIdlePolicy(c)
	v.checkPrivacyTier(c)
	v.checkSpend(c)
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

func (v *validator) checkRequired(c *Config) {
	if c.Role == "" {
		v.add("role", "must be non-empty")
	}
	if c.Tracker == "" {
		v.add("tracker", "must be non-empty")
	}
}

var validRoles = map[string]struct{}{
	"consumer": {},
	"seeder":   {},
	"both":     {},
}

func (v *validator) checkRole(c *Config) {
	if c.Role == "" {
		return // already flagged by checkRequired
	}
	if _, ok := validRoles[c.Role]; !ok {
		v.add("role", "must be one of: consumer, seeder, both")
	}
}

func (v *validator) checkTracker(c *Config) {
	if c.Tracker == "" || c.Tracker == "auto" {
		return
	}
	u, err := url.Parse(c.Tracker)
	if err != nil {
		v.add("tracker", fmt.Sprintf("must be \"auto\" or a URL — %v", err))
		return
	}
	switch u.Scheme {
	case "https", "quic":
		// ok
	default:
		v.add("tracker",
			fmt.Sprintf("URL scheme must be https or quic, got %q", u.Scheme))
	}
	if u.Host == "" {
		v.add("tracker", "URL must include a host")
	}
}

func (v *validator) checkPaths(c *Config) {
	if c.IdentityKeyPath != "" && !filepath.IsAbs(c.IdentityKeyPath) {
		v.add("identity_key_path",
			"must be an absolute path (after ~ expansion)")
	}
	if c.AuditLogPath != "" && !filepath.IsAbs(c.AuditLogPath) {
		v.add("audit_log_path",
			"must be an absolute path (after ~ expansion)")
	}
}

var validSandboxDrivers = map[string]struct{}{
	"bubblewrap": {},
	"firejail":   {},
	"docker":     {},
}

func (v *validator) checkCCBridge(c *Config) {
	if c.CCBridge.ClaudeBin == "" {
		v.add("cc_bridge.claude_bin", "must be non-empty")
	}
	if c.CCBridge.Sandbox.Enabled {
		if _, ok := validSandboxDrivers[c.CCBridge.Sandbox.Driver]; !ok {
			v.add("cc_bridge.sandbox.driver",
				"must be one of: bubblewrap, firejail, docker (when sandbox.enabled is true)")
		}
	}
}

func (v *validator) checkConsumer(c *Config) {
	if c.Consumer.NetworkModeTTL.AsDuration() <= 0 {
		v.add("consumer.network_mode_ttl", "must be > 0")
	}
}

func (v *validator) checkSeeder(c *Config) {
	if c.Seeder.HeadroomWindow.AsDuration() <= 0 {
		v.add("seeder.headroom_window", "must be > 0")
	}
}

var validIdleModes = map[string]struct{}{
	"scheduled": {},
	"always_on": {},
}

// HH:MM-HH:MM with 24-hour clock; both endpoints required.
var idleWindowRe = regexp.MustCompile(`^([0-1]\d|2[0-3]):[0-5]\d-([0-1]\d|2[0-3]):[0-5]\d$`)

func (v *validator) checkIdlePolicy(c *Config) {
	if c.IdlePolicy.Mode == "" {
		// flagged elsewhere only if it's required; default-fill should set this
		v.add("idle_policy.mode", "must be non-empty")
		return
	}
	if _, ok := validIdleModes[c.IdlePolicy.Mode]; !ok {
		v.add("idle_policy.mode",
			"must be one of: scheduled, always_on")
	}
	if c.IdlePolicy.Mode == "scheduled" {
		if c.IdlePolicy.Window == "" {
			v.add("idle_policy.window",
				"must be set when mode=scheduled (HH:MM-HH:MM)")
		} else if !idleWindowRe.MatchString(c.IdlePolicy.Window) {
			v.add("idle_policy.window",
				"must match HH:MM-HH:MM (24-hour clock)")
		}
	}
	if c.IdlePolicy.ActivityGrace.AsDuration() <= 0 {
		v.add("idle_policy.activity_grace", "must be > 0")
	}
}

var validPrivacyTiers = map[string]struct{}{
	"standard":      {},
	"tee_required":  {},
	"tee_preferred": {},
}

func (v *validator) checkPrivacyTier(c *Config) {
	if c.PrivacyTier == "" {
		v.add("privacy_tier", "must be non-empty")
		return
	}
	if _, ok := validPrivacyTiers[c.PrivacyTier]; !ok {
		v.add("privacy_tier",
			"must be one of: standard, tee_required, tee_preferred")
	}
}

func (v *validator) checkSpend(c *Config) {
	if c.MaxSpendPerHour < 0 {
		v.add("max_spend_per_hour", "must be >= 0")
	}
}
