package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/token-bay/token-bay/plugin/internal/hooks"
)

// hooksDefaultTimeout caps how long the hooks subcommand waits on the
// sidecar's local HTTP endpoint. The plugin's design contract is that a
// hook event must never block the host Claude Code turn — five seconds
// is a hard ceiling matching plugin.json's "timeout" hook declaration.
const hooksDefaultTimeout = 5 * time.Second

// hooksTimeoutOverride is a test-only seam so a flaky/slow CI host
// doesn't have to wait the full five-second hook ceiling. Production
// code path uses hooksDefaultTimeout via hooksRequestTimeout().
var hooksTimeoutOverride time.Duration

func hooksRequestTimeout() time.Duration {
	if hooksTimeoutOverride > 0 {
		return hooksTimeoutOverride
	}
	return hooksDefaultTimeout
}

func newHooksCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "hooks <event>",
		Short: "Bridge a Claude Code hook event to the running sidecar",
		Long: `Reads the hook JSON payload from stdin and POSTs it to the running
sidecar's /_hooks/<event> endpoint, copying the sidecar's reply to stdout.

When the sidecar is not running (no $cfgDir/sidecar.url file), the
subprocess silently emits hooks.EmptyResponse and exits 0 — the host
Claude Code turn must never block on a missing supervisor. The same
fail-open behavior applies to connection refusals, 5xx responses, and
5-second timeouts.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if configPath == "" {
				return errors.New("--config is required")
			}
			event := args[0]
			cfgDir := filepath.Dir(configPath)
			return runHooks(cmd.InOrStdin(), cmd.OutOrStdout(), cfgDir, event)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to ~/.token-bay/config.yaml (required)")
	return cmd
}

// runHooks implements the hooks subcommand's I/O contract. Pulled out
// of the cobra closure to keep it directly testable.
//
// Fail-open is the cardinal rule: any error path (missing sidecar.url,
// transport failure, sidecar 5xx, deadline exceeded) writes
// hooks.EmptyResponse to out and returns nil. The only way runHooks
// returns a non-nil error is a write failure on stdout itself, which is
// already terminal for the host process.
func runHooks(stdin io.Reader, stdout io.Writer, cfgDir, event string) error {
	url, err := readDiscoveryFile(cfgDir)
	if err != nil || strings.TrimSpace(url) == "" {
		return hooks.EmptyResponse().Encode(stdout)
	}

	body, err := io.ReadAll(stdin)
	if err != nil {
		return hooks.EmptyResponse().Encode(stdout)
	}

	endpoint := strings.TrimRight(url, "/") + "/_hooks/" + event
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return hooks.EmptyResponse().Encode(stdout)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: hooksRequestTimeout()}
	resp, err := client.Do(req)
	if err != nil {
		return hooks.EmptyResponse().Encode(stdout)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return hooks.EmptyResponse().Encode(stdout)
	}

	if _, err := io.Copy(stdout, resp.Body); err != nil {
		return fmt.Errorf("hooks: copy response: %w", err)
	}
	return nil
}
