package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration for the Token-Bay plugin sidecar.
// Every plugin subsystem reads its section from a *Config produced by
// Load. Required fields (Role, Tracker) have no defaults and must be
// populated by the operator. See the design spec §2.3 for the full
// required-field list.
type Config struct {
	Role            string           `yaml:"role"`
	Tracker         string           `yaml:"tracker"`
	IdentityKeyPath string           `yaml:"identity_key_path"`
	AuditLogPath    string           `yaml:"audit_log_path"`
	CCBridge        CCBridgeConfig   `yaml:"cc_bridge"`
	Consumer        ConsumerConfig   `yaml:"consumer"`
	Seeder          SeederConfig     `yaml:"seeder"`
	IdlePolicy      IdlePolicyConfig `yaml:"idle_policy"`
	PrivacyTier     string           `yaml:"privacy_tier"`
	MaxSpendPerHour int64            `yaml:"max_spend_per_hour"`
}

// CCBridgeConfig configures the Claude Code CLI bridge — the seeder
// role's `claude -p` invocation surface (plugin spec §6.2) and the
// consumer role's `/usage` probe (plugin spec §5.2).
type CCBridgeConfig struct {
	ClaudeBin  string        `yaml:"claude_bin"`
	ExtraFlags []string      `yaml:"extra_flags"`
	Sandbox    SandboxConfig `yaml:"sandbox"`
}

// SandboxConfig is opt-in defense-in-depth for the seeder bridge.
// Disabled by default — plugin spec §6.2 explicitly states that
// flag-based tool-disabling is the primary safety guarantee.
type SandboxConfig struct {
	Enabled bool   `yaml:"enabled"`
	Driver  string `yaml:"driver"`
}

// ConsumerConfig holds knobs that apply only to the consumer role.
type ConsumerConfig struct {
	NetworkModeTTL Duration `yaml:"network_mode_ttl"`
}

// SeederConfig holds knobs that apply only to the seeder role.
type SeederConfig struct {
	HeadroomWindow Duration `yaml:"headroom_window"`
}

// IdlePolicyConfig governs when the seeder advertises available=true
// (plugin spec §6.3).
type IdlePolicyConfig struct {
	Mode          string   `yaml:"mode"`
	Window        string   `yaml:"window"`
	ActivityGrace Duration `yaml:"activity_grace"`
}

// Duration is a yaml-friendly time.Duration: unmarshals from a
// time.ParseDuration-shaped string (`15m`, `2h30m`, `500ms`).
//
// We define a wrapper type because yaml.v3's stock time.Duration
// handling expects a number-of-nanoseconds integer, which is not how
// human operators write config files.
type Duration time.Duration

// AsDuration returns the underlying time.Duration. Callers prefer this
// over the cast in places where readability matters.
func (d Duration) AsDuration() time.Duration { return time.Duration(d) }

// UnmarshalYAML parses a YAML scalar string via time.ParseDuration.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("must be a duration string like \"15m\", got node kind %v", node.Kind)
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", node.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML emits the duration as a human-readable string so a
// round-trip preserves operator intent.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// DefaultConfig returns a Config with every defaultable field populated
// per the design spec §2.3. Required fields (role, tracker) are returned
// zero-valued so Validate flags them when the operator forgets.
func DefaultConfig() *Config {
	return &Config{
		IdentityKeyPath: "~/.token-bay/identity.key",
		AuditLogPath:    "~/.token-bay/audit.log",
		CCBridge: CCBridgeConfig{
			ClaudeBin:  "claude",
			ExtraFlags: []string{},
			Sandbox: SandboxConfig{
				Enabled: false,
				Driver:  "bubblewrap",
			},
		},
		Consumer: ConsumerConfig{
			NetworkModeTTL: Duration(15 * time.Minute),
		},
		Seeder: SeederConfig{
			HeadroomWindow: Duration(15 * time.Minute),
		},
		IdlePolicy: IdlePolicyConfig{
			Mode:          "scheduled",
			Window:        "02:00-06:00",
			ActivityGrace: Duration(10 * time.Minute),
		},
		PrivacyTier:     "standard",
		MaxSpendPerHour: 500,
	}
}
