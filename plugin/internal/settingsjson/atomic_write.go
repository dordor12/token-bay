package settingsjson

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// atomicWriteFile writes content to path via a tempfile + fsync + rename.
// On any filesystem POSIX supports, the caller either sees the pre-state or
// the post-state; there is no partial-write window.
//
// The tempfile is created in the same directory as path so rename(2) is
// a simple metadata op (cross-fs renames are not atomic on Linux).
func atomicWriteFile(path string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	tmpName := fmt.Sprintf(".%s.token-bay-tmp-%d-%d", base, os.Getpid(), time.Now().UnixNano())
	tmpPath := filepath.Join(dir, tmpName)

	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("settingsjson: create tempfile: %w", err)
	}
	cleanupTmp := func() { _ = os.Remove(tmpPath) }

	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		cleanupTmp()
		return fmt.Errorf("settingsjson: write tempfile: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		cleanupTmp()
		return fmt.Errorf("settingsjson: fsync tempfile: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanupTmp()
		return fmt.Errorf("settingsjson: close tempfile: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		cleanupTmp()
		return fmt.Errorf("settingsjson: rename tempfile: %w", err)
	}
	return nil
}

// resolveSymlinkTarget returns the absolute path that should be written to
// when path may be a symlink. If path is not a symlink, returns path unchanged.
// If path is a symlink whose resolved target is outside baseDir, returns
// ErrSymlinkEscapesClaudeDir.
//
// baseDir is typically ~/.claude/ — the security boundary for allowed
// settings.json targets.
func resolveSymlinkTarget(path, baseDir string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("settingsjson: lstat %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}

	target, err := os.Readlink(path)
	if err != nil {
		return "", fmt.Errorf("settingsjson: readlink %s: %w", path, err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	target = filepath.Clean(target)

	baseAbs, err := filepath.Abs(baseDir)
	if err != nil {
		return "", fmt.Errorf("settingsjson: abs %s: %w", baseDir, err)
	}
	baseAbs = filepath.Clean(baseAbs)

	rel, err := filepath.Rel(baseAbs, target)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", fmt.Errorf("%w: target %s is outside %s", ErrSymlinkEscapesClaudeDir, target, baseAbs)
	}
	return target, nil
}
