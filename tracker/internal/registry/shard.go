package registry

import (
	"net/netip"
	"sync"
	"time"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/shared/proto"
)

// shard is the unit of locking inside Registry. Each shard owns a private map
// of records and an RWMutex; the registry routes operations to shards by
// hashing the IdentityID.
type shard struct {
	mu   sync.RWMutex
	recs map[ids.IdentityID]*SeederRecord
}

func newShard() *shard {
	return &shard{recs: make(map[ids.IdentityID]*SeederRecord)}
}

// put stores rec, overwriting any existing entry with the same IdentityID.
// The shard takes its own deep copy so the caller cannot reach into the store
// by mutating the slices they passed in.
func (s *shard) put(rec SeederRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := cloneRecord(rec)
	s.recs[rec.IdentityID] = &stored
}

// get returns a deep copy of the record so callers cannot mutate the store.
func (s *shard) get(id ids.IdentityID) (SeederRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.recs[id]
	if !ok {
		return SeederRecord{}, false
	}
	return cloneRecord(*r), true
}

// delete removes the record. No-op when absent.
func (s *shard) delete(id ids.IdentityID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recs, id)
}

// update applies fn to the record under the shard's write lock. Returns
// ErrUnknownSeeder when no record exists. If fn returns an error, the
// mutation is discarded — the shard rolls back to the pre-call state.
//
// fn runs while the write lock is held; keep it short and never block on I/O
// or other locks from inside it.
func (s *shard) update(id ids.IdentityID, fn func(*SeederRecord) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[id]
	if !ok {
		return ErrUnknownSeeder
	}
	// Operate on a copy so we can roll back on error.
	working := cloneRecord(*r)
	if err := fn(&working); err != nil {
		return err
	}
	s.recs[id] = &working
	return nil
}

// snapshot returns a deep copy of every record currently in the shard.
func (s *shard) snapshot() []SeederRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SeederRecord, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, cloneRecord(*r))
	}
	return out
}

// sweepStale removes every record whose LastHeartbeat is at or before
// staleBefore. Returns the count removed.
func (s *shard) sweepStale(staleBefore time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, r := range s.recs {
		if !r.LastHeartbeat.After(staleBefore) {
			delete(s.recs, id)
			removed++
		}
	}
	return removed
}

// cloneRecord deep-copies the slices in a SeederRecord so the store and
// returned copies do not alias their backing arrays.
func cloneRecord(r SeederRecord) SeederRecord {
	out := r
	out.Capabilities = cloneCapabilities(r.Capabilities)
	out.NetCoords.LocalCandidates = cloneAddrPorts(r.NetCoords.LocalCandidates)
	return out
}

// cloneCapabilities deep-copies the slice fields of a Capabilities so the
// store and the caller do not share backing arrays.
func cloneCapabilities(c Capabilities) Capabilities {
	out := c
	out.Models = cloneStrings(c.Models)
	out.Tiers = cloneTiers(c.Tiers)
	out.Attestation = cloneBytes(c.Attestation)
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneTiers(in []proto.PrivacyTier) []proto.PrivacyTier {
	if in == nil {
		return nil
	}
	out := make([]proto.PrivacyTier, len(in))
	copy(out, in)
	return out
}

func cloneBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}

func cloneAddrPorts(in []netip.AddrPort) []netip.AddrPort {
	if in == nil {
		return nil
	}
	out := make([]netip.AddrPort, len(in))
	copy(out, in)
	return out
}
