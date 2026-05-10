package reputation

import "testing"

func TestSignal_RoleAndPrimary(t *testing.T) {
	cases := []struct {
		sig         SignalKind
		wantRole    Role
		wantPrimary bool
	}{
		{SignalBrokerRequest, RoleConsumer, true},
		{SignalProofRejection, RoleConsumer, true},
		{SignalExhaustionClaim, RoleConsumer, true},
		{SignalCostReportDeviation, RoleSeeder, true},
		{SignalConsumerComplaint, RoleSeeder, true},
		{SignalOfferAccept, RoleSeeder, false},
		{SignalOfferReject, RoleSeeder, false},
		{SignalOfferUnreachable, RoleSeeder, false},
		{SignalSettlementClean, RoleSeeder, false},
		{SignalSettlementSigMissing, RoleSeeder, false},
		{SignalDisputeFiled, RoleConsumer, false},
		{SignalDisputeUpheld, RoleConsumer, false},
	}
	for _, c := range cases {
		if got := c.sig.Role(); got != c.wantRole {
			t.Errorf("%v.Role() = %v, want %v", c.sig, got, c.wantRole)
		}
		if got := c.sig.IsPrimary(); got != c.wantPrimary {
			t.Errorf("%v.IsPrimary() = %v, want %v",
				c.sig, got, c.wantPrimary)
		}
	}
}

func TestRole_String(t *testing.T) {
	if RoleConsumer.String() != "consumer" {
		t.Error("RoleConsumer string mismatch")
	}
	if RoleSeeder.String() != "seeder" {
		t.Error("RoleSeeder string mismatch")
	}
	if RoleAgnostic.String() != "agnostic" {
		t.Error("RoleAgnostic string mismatch")
	}
}
