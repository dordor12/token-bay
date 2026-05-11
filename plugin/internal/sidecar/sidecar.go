package sidecar

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/token-bay/token-bay/plugin/internal/ccproxy"
	"github.com/token-bay/token-bay/plugin/internal/consumerflow"
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

// consumerSettlementAdapter bridges *consumerflow.Coordinator's
// HandleSettlement (which takes a *consumerflow.SettlementRequest) to the
// trackerclient.SettlementHandler interface (which takes a
// *trackerclient.SettlementRequest). The two request structs are
// structurally identical but live in separate packages per the
// consumerflow CLAUDE.md "mirrored types" rule — this adapter is the
// single seam.
type consumerSettlementAdapter struct {
	inner *consumerflow.Coordinator
}

func (a consumerSettlementAdapter) HandleSettlement(ctx trackerclient.Ctx, r *trackerclient.SettlementRequest) error {
	cc, ok := ctx.(context.Context)
	if !ok {
		cc = ctxAdapter{Ctx: ctx}
	}
	return a.inner.HandleSettlement(cc, &consumerflow.SettlementRequest{
		PreimageHash: r.PreimageHash,
		PreimageBody: r.PreimageBody,
	})
}

// ctxAdapter satisfies context.Context from a trackerclient.Ctx whose
// concrete type is something other than context.Context (defensive — in
// the production path trackerclient always passes a real context.Context,
// but the adapter remains correct if that ever changes).
type ctxAdapter struct{ trackerclient.Ctx }

func (ctxAdapter) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctxAdapter) Value(any) any               { return nil }

// New validates deps and constructs the trackerclient + ccproxy. The
// returned App has not started any goroutines.
func New(deps Deps) (*App, error) {
	if err := deps.Validate(); err != nil {
		return nil, err
	}

	var tracker *trackerclient.Client
	if deps.TrackerClient != nil {
		tracker = deps.TrackerClient
	} else {
		tcCfg := trackerclient.Config{
			Endpoints: deps.TrackerEndpoints,
			Identity:  deps.Signer,
			Logger:    deps.Logger,
		}
		if deps.SeederFlow != nil {
			tcCfg.OfferHandler = deps.SeederFlow
		}
		if deps.ConsumerFlow != nil {
			tcCfg.SettlementHandler = consumerSettlementAdapter{inner: deps.ConsumerFlow}
		}
		if deps.TrackerClientOverrides != nil {
			deps.TrackerClientOverrides(&tcCfg)
		}
		var err error
		tracker, err = trackerclient.New(tcCfg)
		if err != nil {
			return nil, fmt.Errorf("sidecar: build trackerclient: %w", err)
		}
	}

	proxyOpts := []ccproxy.Option{ccproxy.WithAddr(deps.CCProxyAddr)}
	if deps.SessionStore != nil {
		proxyOpts = append(proxyOpts, ccproxy.WithSessionStore(deps.SessionStore))
	}
	proxy := ccproxy.New(proxyOpts...)

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

	// SeederFlow is optional. When set, run it in a background
	// goroutine — it owns its own listener accept loop, advertise
	// heartbeat, and conformance gate, all bounded by ctx.
	if a.deps.SeederFlow != nil {
		go func() {
			if err := a.deps.SeederFlow.Run(ctx); err != nil {
				a.deps.Logger.Warn().Err(err).Msg("sidecar: seederflow run")
			}
		}()
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
	if a.deps.SeederFlow != nil {
		if err := a.deps.SeederFlow.Close(); err != nil {
			errs = append(errs, fmt.Errorf("seederflow close: %w", err))
		}
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
