package federation_test

import (
	"context"
	"crypto/ed25519"
	crand "crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/federation"
)

func mkID(t *testing.T) (ids.TrackerID, ed25519.PublicKey) {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(crand.Reader)
	return ids.TrackerID(sha256.Sum256(pub)), pub
}

func TestRegistry_AddLookupRemove(t *testing.T) {
	t.Parallel()
	r := federation.NewRegistry()
	id, pub := mkID(t)
	if err := r.Add(federation.PeerInfo{TrackerID: id, PubKey: pub, Addr: "x"}); err != nil {
		t.Fatal(err)
	}
	got, ok := r.Get(id)
	if !ok || !ed25519.PublicKey(got.PubKey).Equal(pub) {
		t.Fatalf("Get miss or pub mismatch")
	}
	if err := r.Depeer(id, federation.ReasonEquivocation); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get(id); ok {
		t.Fatal("Get after Depeer should miss")
	}
}

func TestRegistry_All_ReturnsCopy(t *testing.T) {
	t.Parallel()
	r := federation.NewRegistry()
	id1, pub1 := mkID(t)
	id2, pub2 := mkID(t)
	_ = r.Add(federation.PeerInfo{TrackerID: id1, PubKey: pub1, Addr: "a"})
	_ = r.Add(federation.PeerInfo{TrackerID: id2, PubKey: pub2, Addr: "b"})
	all := r.All()
	if len(all) != 2 {
		t.Fatalf("All() = %d entries, want 2", len(all))
	}
	all[0].Addr = "tampered"
	got, _ := r.Get(id1)
	if got.Addr == "tampered" {
		t.Fatal("All() must return a copy, not aliased state")
	}
}

func TestRegistry_Add_Duplicate(t *testing.T) {
	t.Parallel()
	r := federation.NewRegistry()
	id, pub := mkID(t)
	_ = r.Add(federation.PeerInfo{TrackerID: id, PubKey: pub, Addr: "x"})
	if err := r.Add(federation.PeerInfo{TrackerID: id, PubKey: pub, Addr: "x"}); err == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestRegistry_All_DeepCopiesPubKey(t *testing.T) {
	t.Parallel()
	r := federation.NewRegistry()
	id, pub := mkID(t)
	if err := r.Add(federation.PeerInfo{TrackerID: id, PubKey: pub, Addr: "x"}); err != nil {
		t.Fatal(err)
	}
	all := r.All()
	all[0].PubKey[0] ^= 0xff // tamper
	got, _ := r.Get(id)
	if got.PubKey[0] != pub[0] {
		t.Fatal("All() must deep-copy PubKey, not alias backing array")
	}
}

func TestRegistry_All_NilsConn(t *testing.T) {
	t.Parallel()
	r := federation.NewRegistry()
	id, pub := mkID(t)
	// Add a peer with a non-nil conn-shaped placeholder to verify it is
	// nilled in the snapshot. We don't have a real PeerConn to hand in
	// here, so use a small fake.
	stub := stubConn{}
	if err := r.Add(federation.PeerInfo{TrackerID: id, PubKey: pub, Addr: "x", Conn: stub}); err != nil {
		t.Fatal(err)
	}
	all := r.All()
	if all[0].Conn != nil {
		t.Fatal("All() must nil the Conn in returned snapshots")
	}
}

// stubConn is a no-op PeerConn used only by TestRegistry_All_NilsConn.
type stubConn struct{}

func (stubConn) Send(context.Context, []byte) error   { return nil }
func (stubConn) Recv(context.Context) ([]byte, error) { return nil, nil }
func (stubConn) RemoteAddr() string                   { return "" }
func (stubConn) RemotePub() ed25519.PublicKey         { return nil }
func (stubConn) Close() error                         { return nil }

func TestRegistry_Add_DuplicateReturnsErrPeerExists(t *testing.T) {
	t.Parallel()
	r := federation.NewRegistry()
	id, pub := mkID(t)
	_ = r.Add(federation.PeerInfo{TrackerID: id, PubKey: pub, Addr: "x"})
	err := r.Add(federation.PeerInfo{TrackerID: id, PubKey: pub, Addr: "x"})
	if !errors.Is(err, federation.ErrPeerExists) {
		t.Fatalf("expected ErrPeerExists, got %v", err)
	}
}
