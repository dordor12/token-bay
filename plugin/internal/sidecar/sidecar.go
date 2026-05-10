package sidecar

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/ccproxy"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

// App is the running supervisor. Construct via New, drive via Run, release
// via Close.
type App struct {
	deps Deps

	tracker *trackerclient.Client
	proxy   *ccproxy.Server

	startMu   sync.Mutex
	started   bool
	closed    bool
	startedAt time.Time
}

// New validates deps and constructs the trackerclient + ccproxy. The
// returned App has not started any goroutines.
func New(deps Deps) (*App, error) {
	if err := deps.Validate(); err != nil {
		return nil, err
	}

	tcCfg := trackerclient.Config{
		Endpoints: deps.TrackerEndpoints,
		Identity:  deps.Signer,
		Logger:    deps.Logger,
	}
	if deps.TrackerClientOverrides != nil {
		deps.TrackerClientOverrides(&tcCfg)
	}
	tracker, err := trackerclient.New(tcCfg)
	if err != nil {
		return nil, fmt.Errorf("sidecar: build trackerclient: %w", err)
	}

	proxy := ccproxy.New(ccproxy.WithAddr(deps.CCProxyAddr))

	return &App{
		deps:    deps,
		tracker: tracker,
		proxy:   proxy,
	}, nil
}

// Run starts every subsystem and blocks until ctx is cancelled or any
// startup step fails. Subsystem startup is synchronous: a ccproxy bind
// error or a trackerclient.Start error returns directly. Background
// loops own their own reconnect; a mid-run subsystem failure does not
// abort the supervisor.
//
// Shutdown order on exit: ccproxy → trackerclient → auditlog.
func (a *App) Run(ctx context.Context) error {
	a.startMu.Lock()
	if a.closed {
		a.startMu.Unlock()
		return ErrClosed
	}
	if a.started {
		a.startMu.Unlock()
		return ErrAlreadyStarted
	}
	a.started = true
	a.startedAt = time.Now()
	a.startMu.Unlock()

	if err := a.proxy.Start(ctx); err != nil {
		return fmt.Errorf("sidecar: start ccproxy: %w", err)
	}

	if err := a.tracker.Start(ctx); err != nil {
		// ccproxy is already running; tear it down before returning.
		_ = closeIgnoreServerClosed(a.proxy)
		return fmt.Errorf("sidecar: start trackerclient: %w", err)
	}

	// Janitor is optional. When set, run it in a background goroutine
	// for the lifetime of ctx; it stops on ctx.Done. The janitor itself
	// recovers from per-scan errors silently (see ccbridge.Janitor.Scan)
	// so a stuck folder won't crash the supervisor.
	if a.deps.Janitor != nil {
		go a.deps.Janitor.Run(ctx)
	}

	// ConsumerFlow is optional. When non-nil, run its periodic
	// network-mode reaper alongside other subsystems. Run returns nil
	// on ctx-cancel; we ignore the return for parity with Janitor.
	if a.deps.ConsumerFlow != nil {
		go func() { _ = a.deps.ConsumerFlow.Run(ctx) }()
	}

	<-ctx.Done()

	return a.shutdown()
}

func (a *App) shutdown() error {
	var errs []error
	if err := closeIgnoreServerClosed(a.proxy); err != nil {
		errs = append(errs, fmt.Errorf("ccproxy close: %w", err))
	}
	if err := a.tracker.Close(); err != nil {
		errs = append(errs, fmt.Errorf("trackerclient close: %w", err))
	}
	if err := a.deps.AuditLog.Close(); err != nil {
		errs = append(errs, fmt.Errorf("auditlog close: %w", err))
	}
	return errors.Join(errs...)
}

// closeIgnoreServerClosed swallows http.ErrServerClosed, which
// ccproxy.Close returns harmlessly if the embedded ctx-cancel goroutine
// already shut the http.Server down.
func closeIgnoreServerClosed(p *ccproxy.Server) error {
	err := p.Close()
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Close stops the supervisor. Safe to call before or after Run; safe to
// call concurrently with Run as long as ctx is cancelled separately. If
// Run never started, Close releases the audit log fd (the only resource
// owned solely by Deps that the supervisor would otherwise leak).
func (a *App) Close() error {
	a.startMu.Lock()
	if a.closed {
		a.startMu.Unlock()
		return nil
	}
	a.closed = true
	started := a.started
	a.startMu.Unlock()

	if !started {
		return a.deps.AuditLog.Close()
	}
	// If Run is in flight, the caller is responsible for cancelling its
	// ctx; shutdown is invoked from Run itself.
	return nil
}
