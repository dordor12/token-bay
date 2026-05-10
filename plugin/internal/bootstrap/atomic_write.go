package bootstrap

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	"github.com/token-bay/token-bay/shared/ids"
)

// atomicWriteFile mirrors plugin/internal/settingsjson's atomicWriteFile
// pattern: tempfile in the same directory, fsync, rename. Local because
// the settingsjson helper is unexported; duplicating five lines beats
// elevating an internal helper to a shared module.
func atomicWriteFile(path string, content []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("bootstrap: mkdir parent of %s: %w", path, err)
	}

	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmpName := fmt.Sprintf(".%s.token-bay-tmp-%d-%d", base, os.Getpid(), time.Now().UnixNano())
	tmpPath := filepath.Join(dir, tmpName)

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("bootstrap: create tempfile: %w", err)
	}
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("bootstrap: write tempfile: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanup()
		return fmt.Errorf("bootstrap: fsync tempfile: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return fmt.Errorf("bootstrap: close tempfile: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("bootstrap: rename tempfile: %w", err)
	}
	return nil
}

func encodePersisted(p trackerclient.BootstrapPeer) persistedPeer {
	return persistedPeer{
		TrackerID:   hex.EncodeToString(p.TrackerID[:]),
		Addr:        p.Addr,
		RegionHint:  p.RegionHint,
		HealthScore: p.HealthScore,
		LastSeen:    p.LastSeen.Unix(),
	}
}

func decodePersisted(p persistedPeer) (trackerclient.BootstrapPeer, bool) {
	raw, err := hex.DecodeString(p.TrackerID)
	if err != nil || len(raw) != 32 {
		return trackerclient.BootstrapPeer{}, false
	}
	if p.Addr == "" {
		return trackerclient.BootstrapPeer{}, false
	}
	var id ids.IdentityID
	copy(id[:], raw)
	return trackerclient.BootstrapPeer{
		TrackerID:   id,
		Addr:        p.Addr,
		RegionHint:  p.RegionHint,
		HealthScore: p.HealthScore,
		LastSeen:    time.Unix(p.LastSeen, 0),
	}, true
}
