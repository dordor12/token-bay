package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// discoveryFilename returns the canonical sidecar.url location under cfgDir.
// The plugin's hooks subcommand reads this file to find the running
// sidecar's ccproxy URL (which is bound on :0 and only known after Start).
func discoveryFilename(cfgDir string) string {
	return filepath.Join(cfgDir, "sidecar.url")
}

// writeDiscoveryFile atomically writes url into the canonical
// sidecar.url path under cfgDir. The temp+rename dance ensures the hooks
// subcommand never sees a half-written URL even if it races with the
// supervisor's startup goroutine.
func writeDiscoveryFile(cfgDir, url string) error {
	path := discoveryFilename(cfgDir)
	tmp, err := os.CreateTemp(cfgDir, ".sidecar.url.*")
	if err != nil {
		return fmt.Errorf("discovery: temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.WriteString(url); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("discovery: write: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("discovery: chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("discovery: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("discovery: rename: %w", err)
	}
	cleanup = false
	return nil
}

// readDiscoveryFile returns the URL stored under cfgDir/sidecar.url. On
// ENOENT the returned error satisfies os.IsNotExist — callers (e.g. the
// hooks subcommand) use that signal to fall back to hooks.EmptyResponse.
func readDiscoveryFile(cfgDir string) (string, error) {
	raw, err := os.ReadFile(discoveryFilename(cfgDir))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// removeDiscoveryFile deletes cfgDir/sidecar.url. Returns nil on absent
// file so the caller (run_cmd's defer) need not check existence.
func removeDiscoveryFile(cfgDir string) error {
	err := os.Remove(discoveryFilename(cfgDir))
	if err == nil || errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// writeDiscoveryFileWhenReady polls urlFn until it returns a non-empty
// value, then writes that URL to cfgDir/sidecar.url. Exits on ctx
// cancel without writing if the URL never materialises.
//
// Designed to be spawned as a goroutine alongside sidecar.App.Run —
// ccproxy binds on :0 so the resolved URL is only known once Start
// completes, but the cmd layer cannot block on Start (it must call
// Run, which blocks until ctx cancel). The polling loop is the seam.
func writeDiscoveryFileWhenReady(ctx context.Context, cfgDir string, urlFn func() string) {
	const pollInterval = 25 * time.Millisecond
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		if url := urlFn(); url != "" {
			_ = writeDiscoveryFile(cfgDir, url)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
