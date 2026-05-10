package api

import (
	"context"
	"net/netip"
	"time"

	"github.com/rs/zerolog"

	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/broker"
)

// idsLike is a single-source-of-truth alias so per-handler files don't
// each need to import shared/ids.
type idsLike = ids.IdentityID

// LedgerService is the union of every ledger method any handler needs.
// *ledger.Ledger satisfies it structurally. Per-handler interfaces
// (balanceLedger, enrollLedger, …) declared in their own files keep the
// dependency narrow at call sites.
type LedgerService interface {
	balanceLedger
	enrollLedger
}

// RegistryService is the union of every registry method any handler
// needs. *registry.Registry satisfies it structurally.
type RegistryService interface {
	advertiseRegistry
}

// StunTurnService is the union of every stunturn method any handler
// needs. The cmd/run_cmd composition root wires a small adapter struct
// that delegates to the stunturn package's Allocator and the package-
// level Reflect helper.
type StunTurnService interface {
	stunReflector
	turnAllocator
}

// BrokerService is the slice of broker used by the broker_request handler.
// *broker.Broker satisfies it structurally.
type BrokerService interface {
	Submit(ctx context.Context, env *tbproto.EnvelopeSigned) (*broker.Result, error)
	RegisterQueued(env *tbproto.EnvelopeSigned, requestID [16]byte, deliver func(*broker.Result))
}

// SettlementService is the union of every settlement method any handler needs.
// *broker.Settlement satisfies it structurally.
type SettlementService interface {
	usageReportHandler
	settleHandler
}

// AdmissionService is the slice of admission used by the enroll handler and
// broker_request handler.
type AdmissionService interface {
	enrollAdmission
	brokerAdmission
}

// FederationService is reserved for the federation subsystem
// (Scope-2 stub).
type FederationService interface {
	federationStartTransfer
}

// ReputationRecorder is the slice of reputation used by the
// broker_request handler. *reputation.Subsystem satisfies it
// structurally.
type ReputationRecorder interface {
	RecordBrokerRequest(consumer ids.IdentityID, decision string) error
}

// Deps lists the subsystems an api handler may depend on. Fields left
// nil cause the matching RPCs to register as ErrNotImplemented stubs.
type Deps struct {
	Logger zerolog.Logger
	Now    func() time.Time
	Debug  bool // surface internal-error messages on responses

	Ledger           LedgerService
	Registry         RegistryService
	StunTurn         StunTurnService
	Broker           BrokerService
	Settlement       SettlementService
	Admission        AdmissionService
	Federation       FederationService
	Reputation       ReputationRecorder
	BootstrapPeers   BootstrapPeersService
	BootstrapMetrics BootstrapPeersMetrics // optional; nil → no observability
}

// RequestCtx carries per-call info every handler may need.
type RequestCtx struct {
	PeerID     idsLike
	RemoteAddr netip.AddrPort
	Now        time.Time
	Logger     zerolog.Logger
}

// handlerFunc is the per-RPC dispatch closure. It returns either a
// fully-formed RpcResponse (for handlers that build their own response,
// e.g. the per-method real handlers) or an error (let Dispatch run it
// through ErrToResponse).
type handlerFunc func(ctx context.Context, rc *RequestCtx, payload []byte) (*tbproto.RpcResponse, error)

// Router maps RpcMethod → handler. Stateless after construction;
// Dispatch is safe under arbitrary concurrency.
type Router struct {
	deps     Deps
	handlers map[tbproto.RpcMethod]handlerFunc
}

// NewRouter builds the dispatch table. Each handler's install* function
// inspects the matching Deps.X field; nil → ErrNotImplemented stub.
func NewRouter(d Deps) (*Router, error) {
	if d.Now == nil {
		d.Now = time.Now
	}
	r := &Router{deps: d, handlers: map[tbproto.RpcMethod]handlerFunc{}}
	r.handlers[tbproto.RpcMethod_RPC_METHOD_ENROLL] = r.installEnroll()
	r.handlers[tbproto.RpcMethod_RPC_METHOD_BROKER_REQUEST] = r.installBrokerRequest()
	r.handlers[tbproto.RpcMethod_RPC_METHOD_BALANCE] = r.installBalance()
	r.handlers[tbproto.RpcMethod_RPC_METHOD_SETTLE] = r.installSettle()
	r.handlers[tbproto.RpcMethod_RPC_METHOD_USAGE_REPORT] = r.installUsageReport()
	r.handlers[tbproto.RpcMethod_RPC_METHOD_ADVERTISE] = r.installAdvertise()
	r.handlers[tbproto.RpcMethod_RPC_METHOD_TRANSFER_REQUEST] = r.installTransferRequest()
	r.handlers[tbproto.RpcMethod_RPC_METHOD_STUN_ALLOCATE] = r.installStunAllocate()
	r.handlers[tbproto.RpcMethod_RPC_METHOD_TURN_RELAY_OPEN] = r.installTurnRelayOpen()
	r.handlers[tbproto.RpcMethod_RPC_METHOD_BOOTSTRAP_PEERS] = r.installBootstrapPeers()
	return r, nil
}

// Dispatch is the single entry point. It NEVER returns an error:
// protocol errors become *RpcResponse with non-OK status. Transport
// errors are server's concern.
func (r *Router) Dispatch(ctx context.Context, rc *RequestCtx, req *tbproto.RpcRequest) *tbproto.RpcResponse {
	if req.Method == tbproto.RpcMethod_RPC_METHOD_UNSPECIFIED {
		return &tbproto.RpcResponse{
			Status: tbproto.RpcStatus_RPC_STATUS_INVALID,
			Error: &tbproto.RpcError{
				Code:    "METHOD_ZERO_RESERVED",
				Message: "method 0 reserved for heartbeat channel",
			},
		}
	}
	h, ok := r.handlers[req.Method]
	if !ok {
		return &tbproto.RpcResponse{
			Status: tbproto.RpcStatus_RPC_STATUS_INVALID,
			Error: &tbproto.RpcError{
				Code:    "UNKNOWN_METHOD",
				Message: req.Method.String(),
			},
		}
	}
	resp, err := h(ctx, rc, req.Payload)
	if err != nil {
		return ErrToResponse(err, r.deps.Debug)
	}
	if resp == nil {
		return OkResponse(nil)
	}
	return resp
}

// PushAPI returns the encoder/decoder helpers for push streams.
func (r *Router) PushAPI() PushAPI { return PushAPI{} }

// notImpl returns a closure that always reports ErrNotImplemented(rpc).
// Used by every install* function when the matching Deps.X is nil.
func notImpl(rpc string) handlerFunc {
	return func(_ context.Context, _ *RequestCtx, _ []byte) (*tbproto.RpcResponse, error) {
		return nil, ErrNotImplemented(rpc)
	}
}
