package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiscoveryFilename_JoinsCfgDir(t *testing.T) {
	got := discoveryFilename("/tmp/cfg")
	assert.Equal(t, filepath.Join("/tmp/cfg", "sidecar.url"), got)
}

func TestWriteDiscoveryFile_AtomicRoundTrip(t *testing.T) {
	dir := t.TempDir()
	url := "http://127.0.0.1:54321/"

	require.NoError(t, writeDiscoveryFile(dir, url))

	got, err := readDiscoveryFile(dir)
	require.NoError(t, err)
	assert.Equal(t, url, got)

	// File mode should be user-only (0600) on POSIX systems. Windows
	// doesn't honor POSIX bits — Mode().Perm() returns 0o666 there —
	// so the assertion only runs where it's meaningful.
	if runtime.GOOS != "windows" {
		st, err := os.Stat(discoveryFilename(dir))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
	}
}

func TestWriteDiscoveryFile_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, writeDiscoveryFile(dir, "http://127.0.0.1:1/"))
	require.NoError(t, writeDiscoveryFile(dir, "http://127.0.0.1:2/"))

	got, err := readDiscoveryFile(dir)
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:2/", got)
}

func TestReadDiscoveryFile_ENOENTReturnsErrIsNotExist(t *testing.T) {
	dir := t.TempDir()
	_, err := readDiscoveryFile(dir)
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err), "want IsNotExist, got %v", err)
}

func TestRemoveDiscoveryFile_Idempotent(t *testing.T) {
	dir := t.TempDir()
	// Remove on absent file — must not error.
	require.NoError(t, removeDiscoveryFile(dir))

	// Write then remove — must not error and must clear the file.
	require.NoError(t, writeDiscoveryFile(dir, "http://127.0.0.1:1/"))
	require.NoError(t, removeDiscoveryFile(dir))
	_, err := os.Stat(discoveryFilename(dir))
	assert.True(t, os.IsNotExist(err))

	// Second remove is still a no-op.
	require.NoError(t, removeDiscoveryFile(dir))
}

// fakeApp satisfies the writeDiscoveryFileWhenReady "app" parameter. The
// real sidecar.App has the same method via Status(). Pulling the
// dependency out of the test lets discovery_test live without the heavy
// supervisor setup.
type fakeApp struct {
	urls chan string
}

func (f *fakeApp) statusURL() string {
	select {
	case u := <-f.urls:
		return u
	default:
		return ""
	}
}

func TestWriteDiscoveryFileWhenReady_PollsAndWrites(t *testing.T) {
	dir := t.TempDir()
	app := &fakeApp{urls: make(chan string, 1)}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		writeDiscoveryFileWhenReady(ctx, dir, app.statusURL)
		close(done)
	}()

	// Simulate ccproxy binding by feeding a URL to the fake.
	app.urls <- "http://127.0.0.1:9999/"

	require.Eventually(t, func() bool {
		_, err := os.Stat(discoveryFilename(dir))
		return err == nil
	}, time.Second, 10*time.Millisecond)

	cancel()
	<-done

	got, err := readDiscoveryFile(dir)
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:9999/", got)
}

func TestWriteDiscoveryFileWhenReady_ExitsOnCtxCancel(t *testing.T) {
	dir := t.TempDir()
	// App will never report a URL, so the goroutine only exits via ctx.
	urlFunc := func() string { return "" }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		writeDiscoveryFileWhenReady(ctx, dir, urlFunc)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("writeDiscoveryFileWhenReady did not exit on ctx cancel")
	}

	// No discovery file should have been written.
	_, err := os.Stat(discoveryFilename(dir))
	assert.True(t, os.IsNotExist(err))
}
