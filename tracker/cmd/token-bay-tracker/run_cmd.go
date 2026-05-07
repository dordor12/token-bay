package main

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/api"
	"github.com/token-bay/token-bay/tracker/internal/broker"
	"github.com/token-bay/token-bay/tracker/internal/config"
	"github.com/token-bay/token-bay/tracker/internal/ledger"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"github.com/token-bay/token-bay/tracker/internal/registry"
	"github.com/token-bay/token-bay/tracker/internal/server"
	"github.com/token-bay/token-bay/tracker/internal/stunturn"
)

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

			errCh := make(chan error, 1)
			go func() { errCh <- srv.Run(ctx) }()

			select {
			case err := <-errCh:
				return err
			case <-ctx.Done():
				graceCtx, cancel := context.WithTimeout(context.Background(),
					time.Duration(cfg.Server.ShutdownGraceS)*time.Second)
				defer cancel()
				return srv.Shutdown(graceCtx)
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
