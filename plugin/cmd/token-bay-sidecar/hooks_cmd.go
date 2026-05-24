package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/token-bay/token-bay/plugin/internal/hooks"
)

// claudeCodeHookTimeout is Claude Code's own per-hook timeout. The
// subprocess bails before that fires so the host turn always sees an
// EmptyResponse on stdout — never an interrupted half-write.
const claudeCodeHookTimeout = 5 * time.Second

// validHookEvents is the four EventName* constants from the hooks package.
// Keeping the allowlist here (cmd-layer) keeps the subprocess from
// blindly forwarding an arbitrary event name (e.g. a typo in plugin.json)
// to the sidecar — the sidecar's ccproxy /_hooks/{event} would 400 the
// unknown name, but failing fast in the subprocess gives a clearer stderr
// log for the operator.
var validHookEvents = map[string]struct{}{
	hooks.EventNameStopFailure:      {},
	hooks.EventNameSessionStart:     {},
	hooks.EventNameSessionEnd:       {},
	hooks.EventNameUserPromptSubmit: {},
}

// hookSubcommandOpts carries the inputs runHookSubcommand needs. Production
// wiring is in newHooksCmd; tests construct this directly with bytes.Buffer
// stdin/stdout to assert IPC behavior end-to-end (sans cobra parsing).
type hookSubcommandOpts struct {
	CfgDir    string
	EventName string
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer
	Timeout   time.Duration
}

// runHookSubcommand performs the subprocess's three steps:
//  1. Read the discovery file (cfgDir/sidecar.url). Missing → exit 0 with
//     an EmptyResponse on stdout; stderr-log so the operator sees the gap.
//  2. POST stdin to <sidecar>/_hooks/{event} with a 5s budget. Sidecar
//     errors (5xx, transport failure, timeout) → exit 0 with EmptyResponse
//     on stdout; stderr-log the cause.
//  3. On success, copy the sidecar's response body to stdout verbatim.
//
// The function ALWAYS returns nil. The hook contract from plugin spec §2.1
// and plugin/internal/hooks/dispatcher.go is non-negotiable: hook
// observation MUST NEVER block the host Claude Code turn. Any non-nil
// return from this function would bubble out as a non-zero exit code and
// confuse the host's hook executor.
func runHookSubcommand(ctx context.Context, opts hookSubcommandOpts) error {
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}
	if opts.Timeout == 0 {
		opts.Timeout = claudeCodeHookTimeout
	}

	if _, ok := validHookEvents[opts.EventName]; !ok {
		fmt.Fprintf(opts.Stderr, "token-bay-sidecar hooks: unknown event %q (expected one of StopFailure, SessionStart, SessionEnd, UserPromptSubmit)\n", opts.EventName)
		return emitEmptyResponse(opts.Stdout)
	}

	sidecarURL, err := readDiscoveryFile(opts.CfgDir)
	if err != nil {
		// Missing file is the dominant "sidecar not running" path. Other
		// errors (perm denied, disk failure) also degrade to "no sidecar"
		// — the host turn must not be blocked even if the operator's
		// state is broken.
		fmt.Fprintf(opts.Stderr, "token-bay-sidecar hooks: no sidecar.url in %s (%v); host turn continues unaffected\n", opts.CfgDir, err)
		return emitEmptyResponse(opts.Stdout)
	}
	if sidecarURL == "" {
		fmt.Fprintf(opts.Stderr, "token-bay-sidecar hooks: sidecar.url empty in %s; host turn continues unaffected\n", opts.CfgDir)
		return emitEmptyResponse(opts.Stdout)
	}

	endpoint, err := buildHookEndpoint(sidecarURL, opts.EventName)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "token-bay-sidecar hooks: bad sidecar URL %q: %v\n", sidecarURL, err)
		return emitEmptyResponse(opts.Stdout)
	}

	// Drain stdin upfront so we can retry / time out without re-reading
	// a non-seekable reader. Bounded by the host's own stdin size — the
	// dispatcher rejects payloads it can't decode, so unbounded growth
	// here is not a concern in practice.
	stdinBytes, err := io.ReadAll(opts.Stdin)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "token-bay-sidecar hooks: read stdin: %v\n", err)
		return emitEmptyResponse(opts.Stdout)
	}

	reqCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(stdinBytes))
	if err != nil {
		fmt.Fprintf(opts.Stderr, "token-bay-sidecar hooks: build request: %v\n", err)
		return emitEmptyResponse(opts.Stdout)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "token-bay-sidecar hooks: POST %s: %v\n", endpoint, err)
		return emitEmptyResponse(opts.Stdout)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		fmt.Fprintf(opts.Stderr, "token-bay-sidecar hooks: sidecar returned %d on %s\n", resp.StatusCode, endpoint)
		return emitEmptyResponse(opts.Stdout)
	}

	// Copy the sidecar's response body to stdout. The dispatcher always
	// emits a JSON object; we forward it as-is so the host hook executor
	// can parse Response.Continue / Response.SystemMessage / etc. when a
	// future flow starts populating them.
	if _, err := io.Copy(opts.Stdout, resp.Body); err != nil {
		// If we crashed mid-copy, the host may have seen partial JSON.
		// Best we can do is log and not crash the host.
		fmt.Fprintf(opts.Stderr, "token-bay-sidecar hooks: copy response: %v\n", err)
	}
	return nil
}

// emitEmptyResponse writes the no-op JSON Response to stdout so the host
// hook executor sees a clean reply even on internal failure.
func emitEmptyResponse(stdout io.Writer) error {
	if err := hooks.EmptyResponse().Encode(stdout); err != nil {
		// Even this is non-fatal — stdout going away means the host is
		// already torn down, so there's nothing to block.
		return nil
	}
	return nil
}

// buildHookEndpoint composes the POST URL from the discovery file's base
// URL and the event name. Rejects malformed bases so the subprocess
// fails fast instead of issuing a request to an unexpected host.
func buildHookEndpoint(base, event string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" || u.Host == "" {
		return "", errors.New("base URL missing scheme or host")
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + "_hooks/" + event, nil
}

// newHooksCmd is the cobra entry point. Registered on the root cmd in
// main.go so `token-bay-sidecar hooks <event>` shows up in --help next
// to run / enroll / transfer / version.
func newHooksCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "hooks <event-name>",
		Short: "Forward a Claude Code hook event to the running sidecar",
		Long: `Subcommand invoked by Claude Code's hook executor (one subprocess per
event). Reads the hook payload from stdin, POSTs to the running sidecar's
ccproxy /_hooks/{event} endpoint, and copies the response to stdout.

If the sidecar is not running (no sidecar.url discovery file), exits 0
silently with an EmptyResponse on stdout — host Claude Code turns are
NEVER blocked by sidecar absence.

Event name must be one of: StopFailure, SessionStart, SessionEnd,
UserPromptSubmit.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if configPath == "" {
				return errors.New("--config is required")
			}
			cfgDir := filepath.Dir(configPath)
			ctx, stop := signalContext(cmd.Context())
			defer stop()
			// Note: runHookSubcommand always returns nil — the host turn
			// MUST NEVER be blocked by an exit code from this subprocess.
			return runHookSubcommand(ctx, hookSubcommandOpts{
				CfgDir:    cfgDir,
				EventName: args[0],
				Stdin:     cmd.InOrStdin(),
				Stdout:    cmd.OutOrStdout(),
				Stderr:    cmd.ErrOrStderr(),
				Timeout:   claudeCodeHookTimeout,
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to ~/.token-bay/config.yaml (required, used only to find the discovery file directory)")
	return cmd
}

// signalContext returns the parent context untouched. The hook subprocess
// is short-lived (≤5s); no SIGINT/SIGTERM handling needed beyond cobra's
// existing signal propagation. Stub exists so tests can swap if a future
// flow needs custom signal handling.
func signalContext(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	return ctx, cancel
}

// Sentinel error for tests that want to assert "the discovery file does
// not exist" without re-wrapping os.ErrNotExist. Kept here so the test
// surface doesn't have to import os.
var _ = os.ErrNotExist // keep os import used in production builds
