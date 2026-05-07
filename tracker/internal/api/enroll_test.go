package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"

	sharedadmission "github.com/token-bay/token-bay/shared/admission"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/tracker/internal/admission"
	"github.com/token-bay/token-bay/tracker/internal/api"
)

// fakeEnrollLedger satisfies api.LedgerService (balanceLedger ∪
// enrollLedger). The handler only calls IssueStarterGrant; SignedBalance
// is present so the type satisfies the union.
type fakeEnrollLedger struct {
	gotID     []byte
	gotAmount uint64
	retEntry  *tbproto.Entry
	retErr    error
}

func (f *fakeEnrollLedger) IssueStarterGrant(_ context.Context, id []byte, amount uint64) (*tbproto.Entry, error) {
	f.gotID = append([]byte(nil), id...)
	f.gotAmount = amount
	return f.retEntry, f.retErr
}

func (f *fakeEnrollLedger) SignedBalance(_ context.Context, _ []byte) (*tbproto.SignedBalanceSnapshot, error) {
	return nil, errors.New("not used in enroll tests")
}

type fakeAdmission struct {
	called bool
	retErr error
}

func (f *fakeAdmission) Admit(_ context.Context, _, _ []byte) error {
	f.called = true
	return f.retErr
}

// Decide is a no-op stub so *fakeAdmission satisfies api.AdmissionService
// (which now requires brokerAdmission in addition to enrollAdmission).
func (f *fakeAdmission) Decide(_ ids.IdentityID, _ *sharedadmission.SignedCreditAttestation, _ time.Time) admission.Result {
	return admission.Result{Outcome: admission.OutcomeAdmit}
}

// rcForPubkey returns a RequestCtx whose PeerID equals SHA-256(pub) so
// the enroll handler's defense-in-depth check passes.
func rcForPubkey(pub []byte) *api.RequestCtx {
	var id ids.IdentityID
	sum := sha256.Sum256(pub)
	copy(id[:], sum[:])
	return &api.RequestCtx{
		PeerID:     id,
		RemoteAddr: netip.MustParseAddrPort("127.0.0.1:55001"),
		Now:        time.Unix(1700000000, 0),
		Logger:     zerolog.Nop(),
	}
}

func enrollRequestBytes(t *testing.T, pub ed25519.PublicKey) []byte {
	t.Helper()
	b, err := proto.Marshal(&tbproto.EnrollRequest{
		IdentityPubkey:     pub,
		Role:               0x1, // consumer
		AccountFingerprint: bytes.Repeat([]byte{0x33}, 32),
		Nonce:              bytes.Repeat([]byte{0x44}, 16),
		ConsumerSig:        bytes.Repeat([]byte{0x55}, 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEnroll_OK_NoAdmission(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeEnrollLedger{
		retEntry: &tbproto.Entry{Body: &tbproto.EntryBody{Seq: 1}, TrackerSig: bytes.Repeat([]byte{0x11}, 64)},
	}
	r, _ := api.NewRouter(api.Deps{Ledger: fake})

	resp := r.Dispatch(context.Background(), rcForPubkey(pub), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_ENROLL, Payload: enrollRequestBytes(t, pub),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
	var out tbproto.EnrollResponse
	if err := proto.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.StarterGrantCredits != 1000 {
		t.Errorf("credits = %d", out.StarterGrantCredits)
	}
	if len(out.IdentityId) != 32 {
		t.Errorf("identity_id len = %d", len(out.IdentityId))
	}
	if fake.gotAmount != 1000 {
		t.Errorf("ledger called with amount %d", fake.gotAmount)
	}
}

func TestEnroll_AdmissionPasses_LedgerCalled(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeEnrollLedger{retEntry: &tbproto.Entry{Body: &tbproto.EntryBody{}}}
	adm := &fakeAdmission{}
	r, _ := api.NewRouter(api.Deps{Ledger: fake, Admission: adm})

	resp := r.Dispatch(context.Background(), rcForPubkey(pub), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_ENROLL, Payload: enrollRequestBytes(t, pub),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_OK {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
	if !adm.called {
		t.Fatal("Admit not called")
	}
}

func TestEnroll_AdmissionRejects_Internal(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeEnrollLedger{}
	adm := &fakeAdmission{retErr: errors.New("queue full")}
	r, _ := api.NewRouter(api.Deps{Ledger: fake, Admission: adm})

	resp := r.Dispatch(context.Background(), rcForPubkey(pub), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_ENROLL, Payload: enrollRequestBytes(t, pub),
	})
	// Generic admission error → INTERNAL.
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INTERNAL {
		t.Fatalf("status = %v: %+v", resp.Status, resp.Error)
	}
}

func TestEnroll_LedgerError_Internal(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	fake := &fakeEnrollLedger{retErr: errors.New("ledger boom")}
	r, _ := api.NewRouter(api.Deps{Ledger: fake})

	resp := r.Dispatch(context.Background(), rcForPubkey(pub), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_ENROLL, Payload: enrollRequestBytes(t, pub),
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INTERNAL ||
		resp.Error.Code != "INTERNAL" {
		t.Fatalf("got %+v / %+v", resp.Status, resp.Error)
	}
}

func TestEnroll_BadPubkeyLen_Invalid(t *testing.T) {
	fake := &fakeEnrollLedger{}
	r, _ := api.NewRouter(api.Deps{Ledger: fake})
	body, _ := proto.Marshal(&tbproto.EnrollRequest{IdentityPubkey: []byte{0x01}})
	resp := r.Dispatch(context.Background(), newRC(), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_ENROLL, Payload: body,
	})
	if resp.Status != tbproto.RpcStatus_RPC_STATUS_INVALID {
		t.Fatalf("status = %v", resp.Status)
	}
}

func TestEnroll_NilLedger_NotImplemented(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	r, _ := api.NewRouter(api.Deps{})
	resp := r.Dispatch(context.Background(), rcForPubkey(pub), &tbproto.RpcRequest{
		Method: tbproto.RpcMethod_RPC_METHOD_ENROLL, Payload: enrollRequestBytes(t, pub),
	})
	if resp.Error == nil || resp.Error.Code != "NOT_IMPLEMENTED" {
		t.Fatalf("got %+v", resp.Error)
	}
}
