package main

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/admin"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/broker"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"github.com/token-bay/token-bay/tracker/internal/registry"
	"github.com/token-bay/token-bay/tracker/internal/reputation"
	"github.com/token-bay/token-bay/tracker/internal/server"
	"github.com/token-bay/token-bay/tracker/internal/stunturn"
)

// adminTokenEnvVar is the name of the env var holding the admin bearer
// token. Empty / unset rejects every admin request — operators must set
// this explicitly to enable the admin port.
const adminTokenEnvVar = "TOKEN_BAY_ADMIN_TOKEN" //nolint:gosec // env var name, not a credential

// newRunCmd is the composition root: parse --config, build subsystems,
// construct server.Deps, run server.Run, install SIGINT/SIGTERM handler.
func newRunCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Start the tracker server",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configPath == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "error: --config is required")
				exitWithCode(cmd.Context(), 1)
				return nil
			}
			cfg, err := config.Load(configPath)
			if err != nil {
				return reportConfigError(cmd, err)
			}
			logger := newLogger(cfg.LogLevel)

			keyBytes, err := os.ReadFile(cfg.Server.IdentityKeyPath)
			if err != nil {
				return fmt.Errorf("read identity key: %w", err)
			}
			trackerKey := ed25519.PrivateKey(keyBytes)

			store, err := storage.Open(cmd.Context(), cfg.Ledger.StoragePath)
			if err != nil {
				return fmt.Errorf("ledger storage: %w", err)
			}
			defer store.Close()

			led, err := ledger.Open(store, trackerKey)
			if err != nil {
				return fmt.Errorf("ledger: %w", err)
			}

			reg, err := registry.New(registry.DefaultShardCount)
			if err != nil {
				return err
			}

			// Build push proxy before server so broker can be wired
			// before server is constructed (chicken-and-egg resolution).
			pp := &pushProxy{}

			adm, err := admission.Open(
				cfg.Admission, reg, trackerKey,
				admission.WithTLogPath(cfg.Admission.TLogPath),
				admission.WithSnapshotPrefix(cfg.Admission.SnapshotPathPrefix),
			)
			if err != nil {
				return fmt.Errorf("admission: %w", err)
			}
			defer adm.Close() //nolint:errcheck

			// federation: select QUICTransport when ListenAddr is set, else
			// fall back to the in-process transport (which keeps the
			// subsystem dormant — no real network presence — when peers
			// are not configured).
			//
			// Federation must open before reputation so we can pass the
			// resulting *Federation as reputation's FreezeListener (it
			// satisfies the interface via Go structural typing).
			trackerPub := trackerKey.Public().(ed25519.PublicKey)
			var fedTransport federation.Transport
			if cfg.Federation.ListenAddr != "" {
				fedCert, err := federation.CertFromIdentity(trackerKey)
				if err != nil {
					return fmt.Errorf("federation cert: %w", err)
				}
				fedTransport, err = federation.NewQUICTransport(federation.QUICConfig{
					ListenAddr:  cfg.Federation.ListenAddr,
					IdleTimeout: time.Duration(cfg.Federation.IdleTimeoutS) * time.Second,
					Cert:        fedCert,
					HandshakeTO: time.Duration(cfg.Federation.HandshakeTimeoutS) * time.Second,
				})
				if err != nil {
					return fmt.Errorf("federation transport: %w", err)
				}
			} else {
				fedHub := federation.NewInprocHub()
				fedTransport = federation.NewInprocTransport(fedHub, "self", trackerPub, trackerKey)
			}
			fedPeers := make([]federation.AllowlistedPeer, 0, len(cfg.Federation.Peers))
			for _, p := range cfg.Federation.Peers {
				tid, err := hexToTrackerID(p.TrackerID)
				if err != nil {
					return fmt.Errorf("federation peer %q: %w", p.Addr, err)
				}
				pk, err := hexToPubKey(p.PubKey)
				if err != nil {
					return fmt.Errorf("federation peer %q: %w", p.Addr, err)
				}
				fedPeers = append(fedPeers, federation.AllowlistedPeer{
					TrackerID: tid, PubKey: pk, Addr: p.Addr, Region: p.Region,
				})
			}
			fed, err := federation.Open(federation.Config{
				MyTrackerID:      ids.TrackerID(sha256.Sum256(trackerPub)),
				MyPriv:           trackerKey,
				HandshakeTimeout: time.Duration(cfg.Federation.HandshakeTimeoutS) * time.Second,
				DedupeTTL:        time.Duration(cfg.Federation.GossipDedupeTTLS) * time.Second,
				SendQueueDepth:   cfg.Federation.SendQueueDepth,
				GossipRateQPS:    cfg.Federation.GossipRateQPS,
				PublishCadence:   time.Duration(cfg.Federation.PublishCadenceS) * time.Second,
				IdleTimeout:      time.Duration(cfg.Federation.IdleTimeoutS) * time.Second,
				RedialBase:       time.Duration(cfg.Federation.RedialBaseS) * time.Second,
				RedialMax:        time.Duration(cfg.Federation.RedialMaxS) * time.Second,
				Peers:            fedPeers,
			}, federation.Deps{
				Transport:         fedTransport,
				RootSrc:           ledgerRootSourceAdapter{led: led},
				Archive:           storeAsArchive{store: store},
				RevocationArchive: store, // *storage.Store satisfies PeerRevocationArchive
				KnownPeers:        store, // *storage.Store satisfies KnownPeersArchive
				Metrics:           federation.NewMetrics(prometheus.DefaultRegisterer),
				Logger:            logger,
				Now:               time.Now,
			})
			if err != nil {
				return fmt.Errorf("federation: %w", err)
			}
			defer fed.Close() //nolint:errcheck

			rep, err := reputation.Open(cmd.Context(), cfg.Reputation, reputation.WithFreezeListener(fed))
			if err != nil {
				return fmt.Errorf("reputation: %w", err)
			}
			defer rep.Close() //nolint:errcheck

			var prices *broker.PriceTable
			if cfg.Pricing.Models != nil {
				prices = broker.NewPriceTableFromConfig(cfg.Pricing)
			} else {
				prices = broker.DefaultPriceTable()
			}

			// identityProxy satisfies broker.IdentityResolver before the
			// server is constructed. Wire it to the live server via
			// setSrv after server.New returns (same pattern as pushProxy).
			ip := &identityProxy{}

			brokerSubs, err := broker.Open(cfg.Broker, cfg.Settlement, broker.Deps{
				Logger:     logger,
				Now:        time.Now,
				Registry:   reg,
				Ledger:     led,
				Admission:  adm,
				Reputation: rep,
				Pusher:     pp,
				Pricing:    prices,
				TrackerKey: trackerKey,
				Identity:   ip,
			})
			if err != nil {
				return fmt.Errorf("broker: %w", err)
			}
			defer brokerSubs.Close() //nolint:errcheck

			alloc, err := stunturn.NewAllocator(stunturn.AllocatorConfig{
				MaxKbpsPerSeeder: cfg.STUNTURN.TURNRelayMaxKbps,
				SessionTTL:       time.Duration(cfg.STUNTURN.SessionTTLSeconds) * time.Second,
				Now:              time.Now,
				Rand:             crand.Reader,
			})
			if err != nil {
				return err
			}

			// stunturn.Reflect is a STUN binding-response builder; for the
			// stun_allocate handler we only need the observed remote
			// address (QUIC's view). NAT translation is a broker-era
			// concern (spec §10 open question). Identity is correct here
			// for v1.
			reflectFn := func(remote netip.AddrPort) netip.AddrPort { return remote }

			router, err := api.NewRouter(api.Deps{
				Logger:     logger,
				Now:        time.Now,
				Ledger:     led,
				Registry:   reg,
				StunTurn:   stunTurnAdapter{alloc: alloc, reflect: reflectFn},
				Broker:     brokerSubs.Broker,
				Settlement: brokerSubs.Settlement,
				Admission:  admissionAdapter{adm},
				Reputation: rep,
				BootstrapPeers: bootstrapPeersAdapter{
					store:    store,
					issuer:   ids.IdentityID(sha256.Sum256(trackerPub)),
					priv:     trackerKey,
					maxPeers: cfg.Federation.Bootstrap.MaxPeers,
					ttl:      time.Duration(cfg.Federation.Bootstrap.TTLSeconds) * time.Second,
				},
				BootstrapMetrics: api.NewPrometheusBootstrapMetrics(prometheus.DefaultRegisterer),
			})
			if err != nil {
				return err
			}

			srv, err := server.New(server.Deps{
				Config:   cfg,
				Logger:   logger,
				Now:      time.Now,
				Registry: reg,
				Ledger:   led,
				StunTurn: alloc,
				Reflect:  reflectFn,
				API:      router,
			})
			if err != nil {
				return err
			}
			// Wire the push proxy and identity proxy to the live server
			// now that it exists.
			pp.setSrv(srv)
			ip.setSrv(srv)

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			// stop() cancels ctx, which drains both the QUIC server and
			// the admin server. /maintenance reuses the same path so an
			// HTTP shutdown trigger is identical to SIGTERM.
			adminSrv, err := buildAdminServer(cfg, logger, srv, reg, led, brokerSubs, adm, stop)
			if err != nil {
				return fmt.Errorf("admin: %w", err)
			}

			errCh := make(chan error, 1)
			go func() { errCh <- srv.Run(ctx) }()

			adminErrCh := make(chan error, 1)
			go func() { adminErrCh <- adminSrv.Run(ctx) }()

			select {
			case err := <-errCh:
				graceCtx, cancel := context.WithTimeout(context.Background(),
					time.Duration(cfg.Server.ShutdownGraceS)*time.Second)
				defer cancel()
				_ = adminSrv.Shutdown(graceCtx)
				return err
			case err := <-adminErrCh:
				graceCtx, cancel := context.WithTimeout(context.Background(),
					time.Duration(cfg.Server.ShutdownGraceS)*time.Second)
				defer cancel()
				_ = srv.Shutdown(graceCtx)
				return err
			case <-ctx.Done():
				graceCtx, cancel := context.WithTimeout(context.Background(),
					time.Duration(cfg.Server.ShutdownGraceS)*time.Second)
				defer cancel()
				shutdownErr := srv.Shutdown(graceCtx)
				_ = adminSrv.Shutdown(graceCtx)
				return shutdownErr
			}
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "Path to tracker.yaml (required)")
	return cmd
}

// stunTurnAdapter glues the package-level stunturn.Reflect helper and
// the *stunturn.Allocator into one struct that satisfies
// api.StunTurnService (stunReflector + turnAllocator).
type stunTurnAdapter struct {
	alloc   *stunturn.Allocator
	reflect func(netip.AddrPort) netip.AddrPort
}

func (a stunTurnAdapter) ReflectAddr(remote netip.AddrPort) netip.AddrPort {
	return a.reflect(remote)
}

func (a stunTurnAdapter) Allocate(consumer, seeder ids.IdentityID, requestID [16]byte, now time.Time) (stunturn.Session, error) {
	return a.alloc.Allocate(consumer, seeder, requestID, now)
}

// admissionAdapter wraps *admission.Subsystem and adds the Admit method
// required by api.AdmissionService (enrollAdmission gate). The gate is
// optional by spec (§ enroll, Scope-2); returning nil here auto-passes all
// enroll requests. When enrollment admission is implemented in a later plan,
// this adapter is replaced by a full implementation on *admission.Subsystem.
type admissionAdapter struct {
	*admission.Subsystem
}

func (admissionAdapter) Admit(_ context.Context, _, _ []byte) error { return nil }

// identityProxy satisfies broker.IdentityResolver. Constructed before the
// server and wired via setSrv after server.New returns, using the same
// chicken-and-egg resolution pattern as pushProxy.
type identityProxy struct {
	srv atomic.Pointer[server.Server]
}

func (p *identityProxy) setSrv(s *server.Server) {
	p.srv.Store(s)
}

func (p *identityProxy) PeerPubkey(id ids.IdentityID) (ed25519.PublicKey, bool) {
	s := p.srv.Load()
	if s == nil {
		return nil, false
	}
	return s.PeerPubkey(id)
}

// pushProxy satisfies broker.PushService. It is constructed before the server
// and wired to it via setSrv after server.New returns, resolving the
// chicken-and-egg dependency between broker (needs a pusher) and server (needs
// a router built from broker).
type pushProxy struct {
	srv atomic.Pointer[server.Server]
}

func (p *pushProxy) setSrv(s *server.Server) {
	p.srv.Store(s)
}

func (p *pushProxy) PushOfferTo(id ids.IdentityID, push *tbproto.OfferPush) (<-chan *tbproto.OfferDecision, bool) {
	s := p.srv.Load()
	if s == nil {
		return nil, false
	}
	return s.PushOfferTo(id, push)
}

func (p *pushProxy) PushSettlementTo(id ids.IdentityID, push *tbproto.SettlementPush) (<-chan *tbproto.SettleAck, bool) {
	s := p.srv.Load()
	if s == nil {
		return nil, false
	}
	return s.PushSettlementTo(id, push)
}

// buildAdminServer assembles the admin HTTP server from the live
// subsystems. The bearer token comes from TOKEN_BAY_ADMIN_TOKEN; an unset
// or empty value rejects every admin request, which is intentional —
// operators must opt in to remote admin access.
func buildAdminServer(
	cfg *config.Config,
	logger zerolog.Logger,
	srv *server.Server,
	reg *registry.Registry,
	led *ledger.Ledger,
	brokerSubs *broker.Subsystems,
	adm *admission.Subsystem,
	stop func(),
) (*admin.Server, error) {
	token := os.Getenv(adminTokenEnvVar)
	validator := func(t string) bool { return token != "" && t == token }
	if token == "" {
		logger.Warn().Str("env", adminTokenEnvVar).
			Msg("admin: token unset, all admin requests will be rejected")
	}

	admissionMount := func(mux *http.ServeMux, guard admin.MuxGuard) {
		adm.RegisterMux(mux, admission.MuxGuard(guard))
	}

	return admin.New(admin.Deps{
		Config:             cfg,
		Logger:             logger,
		Now:                time.Now,
		Version:            version,
		TokenValidator:     validator,
		PeerCounter:        srv,
		Registry:           reg,
		Ledger:             led,
		BrokerMux:          brokerSubs.AdminHandler(),
		AdmissionMount:     admissionMount,
		TriggerMaintenance: stop,
	})
}
