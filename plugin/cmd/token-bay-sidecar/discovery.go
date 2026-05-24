package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// discoveryFilename is the file the running sidecar writes under cfgDir
// to advertise its live ccproxy URL. Per-event hook subprocesses
// (token-bay-sidecar hooks <event>) read it to discover where to POST
// the hook payload. Atomic write + idempotent remove + os.ErrNotExist
// on missing — all three are part of the documented IPC contract that
// keeps the hook subprocess simple and the host Claude Code turn
// unblocked by sidecar absence.
const discoveryFilename = "sidecar.url"

// writeDiscoveryFile writes the URL atomically (temp file in the same
// directory + os.Rename). Same-directory rename is required for atomicity
// on POSIX — a cross-filesystem rename falls back to copy+unlink, which
// is not crash-safe.
//
// The 0o600 mode mirrors the rest of the plugin's per-user state files
// (identity.key, audit.log) — the discovery URL is loopback-only but
// shouldn't be world-readable on a multi-user host.
func writeDiscoveryFile(cfgDir, url string) error {
	if cfgDir == "" {
		return errors.New("discovery: cfgDir empty")
	}
	if url == "" {
		return errors.New("discovery: url empty")
	}
	finalPath := filepath.Join(cfgDir, discoveryFilename)
	tmp := finalPath + ".token-bay-tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, []byte(url), 0o600); err != nil {
		return fmt.Errorf("discovery: write tmp: %w", err)
	}
	if err := os.Rename(tmp, finalPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("discovery: rename: %w", err)
	}
	return nil
}

// readDiscoveryFile returns the URL stored at cfgDir/sidecar.url, or
// os.ErrNotExist when the file is missing. Hook subprocesses use this
// signal to exit 0 silently — a missing sidecar must never block the
// host Claude Code turn.
func readDiscoveryFile(cfgDir string) (string, error) {
	path := filepath.Join(cfgDir, discoveryFilename)
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// removeDiscoveryFile deletes the discovery file. Idempotent — a missing
// file is not an error so graceful shutdown can call this unconditionally.
// Other errors (permission denied, IO failure) are surfaced so the cmd
// layer logs them as a "sidecar shutdown could not clean up" diagnostic.
func removeDiscoveryFile(cfgDir string) error {
	path := filepath.Join(cfgDir, discoveryFilename)
	err := os.Remove(path)
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("discovery: remove: %w", err)
}
