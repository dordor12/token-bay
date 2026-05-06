package api

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"

	tbproto "github.com/token-bay/token-bay/shared/proto"
	"github.com/token-bay/token-bay/shared/signing"
)

// enrollStarterGrantCredits matches admission spec default. Hardcoded
// here because admission isn't wired yet (Scope-2); when admission
// lands, the value flows in via Deps.Admission.
const enrollStarterGrantCredits uint64 = 1000

// enrollLedger is the slice of ledger this handler needs.
type enrollLedger interface {
	IssueStarterGrant(ctx context.Context, identityID []byte, amount uint64) (*tbproto.Entry, error)
}

// enrollAdmission is OPTIONAL — when nil, the gate auto-passes
// (Scope-2 contract).
type enrollAdmission interface {
	Admit(ctx context.Context, identity []byte, accountFingerprint []byte) error
}

// errAdmissionFrozen is the sentinel an admission impl may return to
// signal a frozen identity. The handler maps it to ErrFrozen.
var errAdmissionFrozen = errors.New("frozen")

// installEnroll wires the live enroll handler when Deps.Ledger
// satisfies enrollLedger; otherwise the Scope-2 stub. Admission gating
// runs only when Deps.Admission is non-nil.
func (r *Router) installEnroll() handlerFunc {
	if r.deps.Ledger == nil {
		return notImpl("enroll")
	}
	led, ok := r.deps.Ledger.(enrollLedger)
	if !ok {
		return notImpl("enroll")
	}
	var adm enrollAdmission
	if r.deps.Admission != nil {
		adm, _ = r.deps.Admission.(enrollAdmission) // optional; nil if not satisfied
	}

	return func(ctx context.Context, rc *RequestCtx, payloadBytes []byte) (*tbproto.RpcResponse, error) {
		var req tbproto.EnrollRequest
		if err := proto.Unmarshal(payloadBytes, &req); err != nil {
			return nil, ErrInvalid("EnrollRequest: " + err.Error())
		}
		if len(req.IdentityPubkey) != 32 {
			return nil, ErrInvalid(fmt.Sprintf("identity_pubkey length %d, want 32", len(req.IdentityPubkey)))
		}
		if len(req.AccountFingerprint) != 32 {
			return nil, ErrInvalid(fmt.Sprintf("account_fingerprint length %d, want 32", len(req.AccountFingerprint)))
		}
		// Defense-in-depth: rc.PeerID derived from mTLS SPKI MUST equal
		// SHA-256(identity_pubkey). The handshake already enforces
		// identity = SPKI hash, but we re-check here so a logic bug in
		// the wiring layer doesn't silently bind enroll to the wrong
		// identity.
		if rc.PeerID != hashPubkey(req.IdentityPubkey) {
			return nil, ErrUnauthenticated("enroll: identity_pubkey does not match mTLS peer")
		}

		if adm != nil {
			if err := adm.Admit(ctx, rc.PeerID[:], req.AccountFingerprint); err != nil {
				if errors.Is(err, errAdmissionFrozen) {
					return nil, ErrFrozen(err.Error())
				}
				return nil, fmt.Errorf("enroll: admission: %w", err)
			}
		}

		entry, err := led.IssueStarterGrant(ctx, rc.PeerID[:], enrollStarterGrantCredits)
		if err != nil {
			return nil, fmt.Errorf("enroll: starter_grant: %w", err)
		}
		entryBytes, err := signing.DeterministicMarshal(entry)
		if err != nil {
			return nil, fmt.Errorf("enroll: marshal entry: %w", err)
		}

		out := &tbproto.EnrollResponse{
			IdentityId:          rc.PeerID[:],
			StarterGrantCredits: enrollStarterGrantCredits,
			StarterGrantEntry:   entryBytes,
		}
		respBytes, _ := proto.Marshal(out)
		return OkResponse(respBytes), nil
	}
}
