package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	var configPath string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show running sidecar status (tracker phase, ccproxy URL, audit log)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if configPath == "" {
				return errors.New("--config is required")
			}
			return runStatus(cmd.OutOrStdout(), cmd.ErrOrStderr(), filepath.Dir(configPath), asJSON)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to ~/.token-bay/config.yaml (required)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit raw JSON instead of an aligned table")
	return cmd
}

func runStatus(stdout, stderr io.Writer, cfgDir string, asJSON bool) error {
	url, err := readDiscoveryFile(cfgDir)
	if err != nil || strings.TrimSpace(url) == "" {
		fmt.Fprintln(stderr, "sidecar not running: $cfgDir/sidecar.url is missing or empty")
		return errors.New("sidecar not running")
	}

	endpoint := strings.TrimRight(url, "/") + "/_status"
	resp, err := (&http.Client{Timeout: sidecarHTTPTimeout}).Get(endpoint)
	if err != nil {
		fmt.Fprintf(stderr, "sidecar not reachable: %v\n", err)
		return fmt.Errorf("sidecar not reachable: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if asJSON {
		_, err := stdout.Write(body)
		return err
	}

	var m map[string]any
	if jerr := json.Unmarshal(body, &m); jerr != nil {
		// Body wasn't JSON — print as-is.
		_, _ = stdout.Write(body)
		return nil
	}
	return renderKVTable(stdout, m)
}
