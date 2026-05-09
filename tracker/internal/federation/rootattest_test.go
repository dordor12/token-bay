package federation_test

import (
	"context"
	"sync/atomic"
	"testing"

	fed "github.com/token-bay/token-bay/shared/federation"
	"github.com/token-bay/token-bay/tracker/internal/federation"
	"github.com/token-bay/token-bay/tracker/internal/ledger/storage"
	"google.golang.org/protobuf/proto"
)

// fakeArchive implements federation.PeerRootArchive in-memory.
type fakeArchive struct {
	rows           map[string]storage.PeerRoot // key = trackerID|hour
	conflictOnNext bool
}

func newFakeArchive() *fakeArchive {
	return &fakeArchive{rows: map[string]storage.PeerRoot{}}
}

func (f *fakeArchive) key(id []byte, h uint64) string {
	return string(id) + ":" + string(rune(h))
}

func (f *fakeArchive) PutPeerRoot(_ context.Context, p storage.PeerRoot) error {
	if f.conflictOnNext {
		f.conflictOnNext = false
		return storage.ErrPeerRootConflict
	}
	f.rows[f.key(p.TrackerID, p.Hour)] = p
	return nil
}

func (f *fakeArchive) GetPeerRoot(_ context.Context, id []byte, h uint64) (storage.PeerRoot, bool, error) {
	r, ok := f.rows[f.key(id, h)]
	return r, ok, nil
}

func TestRootAttestApply_Persists(t *testing.T) {
	t.Parallel()
	cli := newPeerCfg(t)
	arch := newFakeArchive()
	forwarded := int32(0)
	forwarder := func(context.Context, fed.Kind, []byte) { atomic.AddInt32(&forwarded, 1) }

	apply := federation.NewRootAttestApplier(arch, forwarder, federation.NowFromTime)

	idBytes := cli.id.Bytes()
	msg := &fed.RootAttestation{TrackerId: idBytes[:], Hour: 100, MerkleRoot: b(32, 7), TrackerSig: b(64, 8)}
	payload, _ := proto.Marshal(msg)
	env, _ := federation.SignEnvelope(cli.priv, idBytes[:], fed.Kind_KIND_ROOT_ATTESTATION, payload)
	if err := apply.Apply(context.Background(), env); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, ok, _ := arch.GetPeerRoot(context.Background(), idBytes[:], 100); !ok {
		t.Fatal("archive missing row")
	}
	if got := atomic.LoadInt32(&forwarded); got != 1 {
		t.Fatalf("forwarded = %d, want 1", got)
	}
}

func TestRootAttestApply_RejectsTrackerIDMismatch(t *testing.T) {
	t.Parallel()
	cli := newPeerCfg(t)
	arch := newFakeArchive()
	apply := federation.NewRootAttestApplier(arch, func(context.Context, fed.Kind, []byte) {}, federation.NowFromTime)

	other := newPeerCfg(t)
	otherID := other.id.Bytes()
	msg := &fed.RootAttestation{TrackerId: otherID[:], Hour: 1, MerkleRoot: b(32, 7), TrackerSig: b(64, 8)}
	payload, _ := proto.Marshal(msg)
	idBytes := cli.id.Bytes()
	env, _ := federation.SignEnvelope(cli.priv, idBytes[:], fed.Kind_KIND_ROOT_ATTESTATION, payload)
	if err := apply.Apply(context.Background(), env); err == nil {
		t.Fatal("expected error: tracker_id mismatch")
	}
}
