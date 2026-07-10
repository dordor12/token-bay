package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
	"github.com/token-bay/token-bay/shared/ids"
)

// Role bitmask values, mirroring identity.Role* and tbproto.EnrollRequest.role.
const (
	RoleConsumer = identity.RoleConsumer
	RoleSeeder   = identity.RoleSeeder
)

// connectTimeout bounds the WaitConnected step. trackerclient.New does not
// dial; Start + WaitConnected must both complete before any RPC.
const connectTimeout = 10 * time.Second

// controlShutdownTimeout bounds the graceful HTTP shutdown on ctx cancel.
const controlShutdownTimeout = 2 * time.Second

// keyFilename is the actor's on-disk Ed25519 seed under --data-dir.
const keyFilename = "identity.key"

// options carries everything the actor lifecycle needs. main builds it from
// flags; tests construct it directly and inject Transport for hermeticity.
type options struct {
	Role     uint32 // consumer=1, seeder=2 (bitmask; "both"=3)
	RoleName string // raw role string, used to fabricate the account fingerprint

	TrackerAddr string
	TrackerHash [32]byte
	Region      string

	// TrackerBAddr/TrackerBHash describe the consumer's cross-region
	// transfer target. Accepted now for flag stability; the transfer flow
	// that consumes them lands in a later task.
	TrackerBAddr string
	TrackerBHash [32]byte

	DataDir  string
	CtrlAddr string

	// Transport is an optional injected transport seam. nil => QUIC (prod).
	// Tests pass a loopback transport wired to a fakeserver.
	Transport trackerclient.Transport

	Logger zerolog.Logger
}

// Actor is the shared consumer/seeder lifecycle: a persistent identity, a
// trackerclient connection, an enrollment, and an HTTP control surface.
// Role-specific request/offer/tunnel logic is layered on in later tasks.
type Actor struct {
	opts   options
	signer *identity.Signer
	client *trackerclient.Client

	ln  net.Listener
	srv *http.Server

	mu        sync.RWMutex
	connected bool
	enrolled  bool
	enrollID  ids.IdentityID
}

// newActor loads-or-generates the identity, constructs the trackerclient,
// and binds the control listener (so CtrlAddr is known before run starts).
// It does NOT dial the tracker or start serving — call run for that.
func newActor(opts options) (*Actor, error) {
	if opts.DataDir == "" {
		return nil, errors.New("actor: DataDir required")
	}
	if opts.CtrlAddr == "" {
		return nil, errors.New("actor: CtrlAddr required")
	}
	if opts.TrackerAddr == "" {
		return nil, errors.New("actor: TrackerAddr required")
	}
	if opts.Role == 0 {
		return nil, errors.New("actor: Role required")
	}

	signer, err := loadOrGenerateIdentity(opts.DataDir)
	if err != nil {
		return nil, err
	}

	cfg := trackerclient.Config{
		Endpoints: []trackerclient.TrackerEndpoint{{
			Addr:         opts.TrackerAddr,
			IdentityHash: opts.TrackerHash,
			Region:       opts.Region,
		}},
		Identity: signer,
		Logger:   opts.Logger,
	}
	// Optional injected transport (tests). Leaving it nil lets
	// trackerclient.New default to the QUIC driver.
	if opts.Transport != nil {
		cfg.Transport = opts.Transport
	}
	client, err := trackerclient.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("actor: build trackerclient: %w", err)
	}

	ln, err := net.Listen("tcp", opts.CtrlAddr)
	if err != nil {
		return nil, fmt.Errorf("actor: bind control listener %q: %w", opts.CtrlAddr, err)
	}

	a := &Actor{
		opts:   opts,
		signer: signer,
		client: client,
		ln:     ln,
	}
	a.srv = &http.Server{
		Handler:           a.mux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return a, nil
}

// CtrlAddr returns the resolved control-server address (host:port), with the
// concrete port filled in even when --ctrl-addr requested port 0.
func (a *Actor) CtrlAddr() string { return a.ln.Addr().String() }

// run serves the control API immediately (so /healthz reports 503 while
// the actor is still coming up), then connects + enrolls, then blocks until
// ctx is cancelled and shuts the control server down gracefully.
func (a *Actor) run(ctx context.Context) error {
	serveErr := make(chan error, 1)
	go func() {
		err := a.srv.Serve(a.ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	if err := a.connectAndEnroll(ctx); err != nil {
		// Best-effort teardown before surfacing the failure.
		a.shutdownControl()
		_ = a.client.Close()
		<-serveErr
		return err
	}

	<-ctx.Done()

	a.shutdownControl()
	_ = a.client.Close()
	return <-serveErr
}

// connectAndEnroll performs the tracker handshake: Start, WaitConnected
// (bounded), then the Enroll RPC with a fabricated account fingerprint.
func (a *Actor) connectAndEnroll(ctx context.Context) error {
	if err := a.client.Start(ctx); err != nil {
		return fmt.Errorf("actor: start trackerclient: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := a.client.WaitConnected(waitCtx); err != nil {
		return fmt.Errorf("actor: connect tracker %s: %w", a.opts.TrackerAddr, err)
	}
	a.setConnected()

	// The tracker does not verify the enroll sig/preimage/fingerprint, so
	// the actor fabricates a deterministic account fingerprint per role
	// (no `claude auth status` probe, no Anthropic key).
	fingerprint := fabricateFingerprint(a.opts.RoleName)
	payload, err := identity.BuildEnrollPayload(a.signer, fingerprint, a.opts.Role)
	if err != nil {
		return fmt.Errorf("actor: build enroll payload: %w", err)
	}

	resp, err := a.client.Enroll(ctx, &trackerclient.EnrollRequest{
		IdentityPubkey:     payload.IdentityPubkey,
		Role:               payload.Role,
		AccountFingerprint: payload.AccountFingerprint,
		Nonce:              payload.Nonce,
		Sig:                payload.Sig,
	})
	if err != nil {
		return fmt.Errorf("actor: enroll RPC: %w", err)
	}
	a.setEnrolled(resp.IdentityID)
	return nil
}

func (a *Actor) shutdownControl() {
	shutCtx, cancel := context.WithTimeout(context.Background(), controlShutdownTimeout)
	defer cancel()
	_ = a.srv.Shutdown(shutCtx)
}

func (a *Actor) setConnected() {
	a.mu.Lock()
	a.connected = true
	a.mu.Unlock()
}

func (a *Actor) setEnrolled(id ids.IdentityID) {
	a.mu.Lock()
	a.enrolled = true
	a.enrollID = id
	a.mu.Unlock()
}

// ready reports whether the actor has both connected and enrolled.
func (a *Actor) ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.connected && a.enrolled
}

// identitySnapshot returns the tracker-issued enroll id and the local pubkey,
// both hex-encoded. The enroll id is the tracker's SPKI-hash identity — NOT
// signer.IdentityID() (= sha256(rawPubkey)) — because later tasks use the
// enroll-returned id as the ConsumerId.
func (a *Actor) identitySnapshot() (identityIDHex, pubkeyHex string) {
	a.mu.RLock()
	id := a.enrollID
	a.mu.RUnlock()
	return hex.EncodeToString(id[:]), hex.EncodeToString(a.signer.PublicKey())
}

// loadOrGenerateIdentity loads <dir>/identity.key, or generates + persists a
// fresh Ed25519 keypair there on first run.
func loadOrGenerateIdentity(dir string) (*identity.Signer, error) {
	keyPath := filepath.Join(dir, keyFilename)
	s, err := identity.LoadKey(keyPath)
	switch {
	case err == nil:
		return s, nil
	case errors.Is(err, identity.ErrKeyNotFound), errors.Is(err, fs.ErrNotExist):
		// fall through to generate
	default:
		return nil, fmt.Errorf("actor: load identity: %w", err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("actor: create data dir: %w", err)
	}
	s, err = identity.Generate()
	if err != nil {
		return nil, fmt.Errorf("actor: generate identity: %w", err)
	}
	if err := identity.SaveKey(keyPath, s); err != nil {
		return nil, fmt.Errorf("actor: save identity: %w", err)
	}
	return s, nil
}

// fabricateFingerprint deterministically derives a 32-byte account
// fingerprint from the role. The tracker does not verify it; it only exists
// to fill the wire field with a well-formed value.
func fabricateFingerprint(role string) [32]byte {
	return sha256.Sum256([]byte("e2e-org-" + role))
}
