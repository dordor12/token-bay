package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/token-bay/token-bay/plugin/internal/hooks"
)

func newFallbackCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "fallback",
		Short: "Manually trigger consumer fallback (POST synthetic StopFailure)",
		Long: `Posts a synthetic StopFailure hook payload to the sidecar's
/_hooks/StopFailure endpoint, exercising the consumer-side fallback
flow without waiting for a real rate-limit event. Used by operators
to verify the consumer path is wired correctly.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				return errors.New("--config is required")
			}
			return runFallback(cmd.OutOrStdout(), cmd.ErrOrStderr(), filepath.Dir(configPath))
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to ~/.token-bay/config.yaml (required)")
	return cmd
}

func runFallback(stdout, stderr io.Writer, cfgDir string) error {
	url, err := readDiscoveryFile(cfgDir)
	if err != nil || strings.TrimSpace(url) == "" {
		fmt.Fprintln(stderr, "sidecar not running: $cfgDir/sidecar.url is missing or empty")
		return errors.New("sidecar not running")
	}

	payload := map[string]any{
		"hook_event_name":  hooks.EventNameStopFailure,
		"session_id":       "synthetic-fallback-cli",
		"transcript_path":  "/tmp/synthetic.jsonl",
		"cwd":              ".",
		"stop_hook_active": true,
		"reason":           "Claude AI usage limit reached|0",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal synthetic payload: %w", err)
	}

	endpoint := strings.TrimRight(url, "/") + "/_hooks/" + hooks.EventNameStopFailure
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: sidecarHTTPTimeout}).Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "sidecar not reachable: %v\n", err)
		return fmt.Errorf("sidecar not reachable: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	fmt.Fprintln(stdout, "fallback: posted synthetic StopFailure; sidecar reply:")
	_, _ = stdout.Write(respBody)
	if !bytes.HasSuffix(respBody, []byte("\n")) {
		fmt.Fprintln(stdout)
	}
	return nil
}
