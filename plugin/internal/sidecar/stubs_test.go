package sidecar

import (
	"context"

	"github.com/token-bay/token-bay/plugin/internal/auditlog"
	"github.com/token-bay/token-bay/plugin/internal/ccproxy"
	"github.com/token-bay/token-bay/plugin/internal/consumerflow"
	"github.com/token-bay/token-bay/plugin/internal/envelopebuilder"
	"github.com/token-bay/token-bay/plugin/internal/exhaustionproofbuilder"
	"github.com/token-bay/token-bay/plugin/internal/settingsjson"
	"github.com/token-bay/token-bay/shared/exhaustionproof"
	"github.com/token-bay/token-bay/shared/ids"
	tbproto "github.com/token-bay/token-bay/shared/proto"
)

// Stubs satisfying the consumerflow collaborator interfaces. None of the
// methods are exercised in the wiring tests — these only exist so
// consumerflow.Deps.Validate() passes and consumerflow.New returns a
// usable Coordinator the wiring assertions can attach.

type stubBroker struct{}

func (stubBroker) BrokerRequest(context.Context, *tbproto.EnvelopeSigned) (*consumerflow.BrokerResult, error) {
	return nil, nil
}

func (stubBroker) BalanceCached(context.Context, ids.IdentityID) (*tbproto.SignedBalanceSnapshot, error) {
	return nil, nil
}
func (stubBroker) Settle(context.Context, []byte, []byte) error { return nil }

type stubSessions struct{}

func (stubSessions) EnterNetworkMode(string, ccproxy.EntryMetadata) {}
func (stubSessions) ExitNetworkMode(string) bool                    { return false }

type stubSettings struct{}

func (stubSettings) GetState(string) (*settingsjson.State, error) { return &settingsjson.State{}, nil }
func (stubSettings) EnterNetworkMode(string, string) error        { return nil }
func (stubSettings) ExitNetworkMode() error                       { return nil }
func (stubSettings) ExitNetworkModeBestEffort(string) error       { return nil }

type stubAudit struct{}

func (stubAudit) LogConsumer(auditlog.ConsumerRecord) error { return nil }

type stubProber struct{}

func (stubProber) Probe(context.Context) ([]byte, error) { return nil, nil }

type stubProofBuilder struct{}

func (stubProofBuilder) Build(exhaustionproofbuilder.ProofInput) (*exhaustionproof.ExhaustionProofV1, error) {
	return nil, nil
}

type stubEnvelopeBuilder struct{}

func (stubEnvelopeBuilder) Build(envelopebuilder.RequestSpec, *exhaustionproof.ExhaustionProofV1, *tbproto.SignedBalanceSnapshot) (*tbproto.EnvelopeSigned, error) {
	return nil, nil
}

type stubIdentity struct{}

func (stubIdentity) IdentityID() ids.IdentityID  { return ids.IdentityID{} }
func (stubIdentity) Sign([]byte) ([]byte, error) { return nil, nil }
