package identity_test

import (
	"testing"

	"github.com/token-bay/token-bay/plugin/internal/identity"
	"github.com/token-bay/token-bay/plugin/internal/trackerclient"
)

// Compile-time check: *identity.Signer satisfies trackerclient.Signer.
var _ trackerclient.Signer = (*identity.Signer)(nil)

func TestSigner_SatisfiesTrackerClientSigner(t *testing.T) {
	s, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	var iface trackerclient.Signer = s
	if iface.IdentityID() != s.IdentityID() {
		t.Fatal("IdentityID round-trip via interface failed")
	}
}
