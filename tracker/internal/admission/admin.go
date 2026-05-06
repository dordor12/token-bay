package admission

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/token-bay/token-bay/shared/ids"
)

// MuxGuard wraps a route handler with auth + audit. Tests inject a guard
// that checks "test-token"; production wires a real bearer-token validator.
type MuxGuard func(http.Handler) http.Handler

// RegisterMux mounts admission's admin handlers under /admission/. All
// routes pass through guard. Plan 3 ships handlers as net/http; mounting
// on a real TLS listener with a real validator belongs to the tracker
// control-plane plan.
func (s *Subsystem) RegisterMux(mux *http.ServeMux, guard MuxGuard) {
	if guard == nil {
		guard = func(h http.Handler) http.Handler { return h }
	}
	mux.Handle("GET /admission/status", guard(http.HandlerFunc(s.handleStatus)))
	mux.Handle("GET /admission/queue", guard(http.HandlerFunc(s.handleQueueList)))
	mux.Handle("GET /admission/consumer/{id}", guard(http.HandlerFunc(s.handleConsumer)))
	mux.Handle("GET /admission/seeder/{id}", guard(http.HandlerFunc(s.handleSeeder)))
	mux.Handle("POST /admission/queue/drain", guard(http.HandlerFunc(s.handleQueueDrain)))
	mux.Handle("POST /admission/queue/eject/{request_id}", guard(http.HandlerFunc(s.handleQueueEject)))
	mux.Handle("POST /admission/snapshot", guard(http.HandlerFunc(s.handleSnapshotForce)))
	mux.Handle("POST /admission/recompute/{consumer_id}", guard(http.HandlerFunc(s.handleRecompute)))
	mux.Handle("GET /admission/peers/blocklist", guard(http.HandlerFunc(s.handleBlocklistList)))
	mux.Handle("POST /admission/peers/blocklist/{peer_id}", guard(http.HandlerFunc(s.handleBlocklistAdd)))
	mux.Handle("DELETE /admission/peers/blocklist/{peer_id}", guard(http.HandlerFunc(s.handleBlocklistRemove)))
}

// BasicAuthGuard returns middleware that checks an "Authorization: Bearer X"
// header; validator decides whether X is a valid operator token.
func BasicAuthGuard(next http.Handler, validator func(token string) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		hdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(hdr, prefix) || !validator(strings.TrimPrefix(hdr, prefix)) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="admission"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// blocklistMu guards Subsystem.blocklist; allocated lazily on first use.
type adminBlocklist struct {
	mu sync.RWMutex
	m  map[ids.IdentityID]struct{}
}

func (s *Subsystem) blocklistAccessor() *adminBlocklist {
	s.adminBlocklistOnce.Do(func() {
		s.adminBlocklist = &adminBlocklist{m: make(map[ids.IdentityID]struct{})}
	})
	return s.adminBlocklist
}

func (s *Subsystem) handleStatus(w http.ResponseWriter, _ *http.Request) {
	snap := s.Supply()
	out := map[string]any{
		"pressure":              snap.Pressure,
		"supply_total_headroom": snap.TotalHeadroom,
		"queue_depth":           s.queue.Len(),
		"thresholds": map[string]float64{
			"admit":  s.cfg.PressureAdmitThreshold,
			"reject": s.cfg.PressureRejectThreshold,
		},
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Subsystem) handleQueueList(w http.ResponseWriter, _ *http.Request) {
	out := s.queueSnapshot()
	writeJSON(w, http.StatusOK, out)
}

func (s *Subsystem) handleConsumer(w http.ResponseWriter, r *http.Request) {
	id, err := parseHexID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	st, ok := consumerShardFor(s.consumerShards, id).get(id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	score, signals := ComputeLocalScore(st, s.cfg, s.nowFn())
	writeJSON(w, http.StatusOK, map[string]any{
		"signals": signals,
		"score":   score,
	})
}

func (s *Subsystem) handleSeeder(w http.ResponseWriter, r *http.Request) {
	id, err := parseHexID(r.PathValue("id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	sh := seederShardFor(s.seederShards, id)
	sh.mu.RLock()
	st, ok := sh.m[id]
	sh.mu.RUnlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	cp := *st
	writeJSON(w, http.StatusOK, &cp)
}

func (s *Subsystem) handleQueueDrain(w http.ResponseWriter, r *http.Request) {
	var body struct {
		N int `json:"n"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	drained := s.drainTopN(body.N)
	if err := s.writeOperatorOverride(r.Context(), "queue/drain", mustJSON(map[string]any{"n": body.N})); err != nil {
		http.Error(w, "tlog write failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"drained": drained})
}

func (s *Subsystem) handleQueueEject(w http.ResponseWriter, r *http.Request) {
	reqID := r.PathValue("request_id")
	ejected := s.ejectQueue(reqID)
	if err := s.writeOperatorOverride(r.Context(), "queue/eject", mustJSON(map[string]any{"request_id": reqID})); err != nil {
		http.Error(w, "tlog write failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ejected": ejected})
}

func (s *Subsystem) handleSnapshotForce(w http.ResponseWriter, r *http.Request) {
	if err := s.runSnapshotEmitOnce(s.nowFn()); err != nil {
		http.Error(w, fmt.Sprintf("snapshot emit failed: %v", err), http.StatusInternalServerError)
		return
	}
	if err := s.writeOperatorOverride(r.Context(), "snapshot", []byte(`{}`)); err != nil {
		http.Error(w, "tlog write failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Subsystem) handleRecompute(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("consumer_id")
	id, err := parseHexID(cid)
	if err != nil {
		http.Error(w, "bad consumer_id", http.StatusBadRequest)
		return
	}
	if _, ok := consumerShardFor(s.consumerShards, id).get(id); !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := s.writeOperatorOverride(r.Context(), "recompute", mustJSON(map[string]any{"consumer_id": cid})); err != nil {
		http.Error(w, "tlog write failed", http.StatusInternalServerError)
		return
	}
	// Real recompute (re-derive from ledger replay) belongs to the ledger
	// event-bus plan; for now, the override record satisfies §9.1.
	writeJSON(w, http.StatusOK, map[string]any{"queued": true})
}

func (s *Subsystem) handleBlocklistList(w http.ResponseWriter, _ *http.Request) {
	bl := s.blocklistAccessor()
	bl.mu.RLock()
	defer bl.mu.RUnlock()
	out := make([]string, 0, len(bl.m))
	for id := range bl.m {
		out = append(out, hex.EncodeToString(id[:]))
	}
	sort.Strings(out)
	writeJSON(w, http.StatusOK, out)
}

func (s *Subsystem) handleBlocklistAdd(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("peer_id")
	id, err := parseHexID(pid)
	if err != nil {
		http.Error(w, "bad peer_id", http.StatusBadRequest)
		return
	}
	bl := s.blocklistAccessor()
	bl.mu.Lock()
	bl.m[id] = struct{}{}
	bl.mu.Unlock()
	if err := s.writeOperatorOverride(r.Context(), "peers/blocklist/add", mustJSON(map[string]any{"peer_id": pid})); err != nil {
		http.Error(w, "tlog write failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Subsystem) handleBlocklistRemove(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("peer_id")
	id, err := parseHexID(pid)
	if err != nil {
		http.Error(w, "bad peer_id", http.StatusBadRequest)
		return
	}
	bl := s.blocklistAccessor()
	bl.mu.Lock()
	delete(bl.m, id)
	bl.mu.Unlock()
	if err := s.writeOperatorOverride(r.Context(), "peers/blocklist/remove", mustJSON(map[string]any{"peer_id": pid})); err != nil {
		http.Error(w, "tlog write failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func parseHexID(s string) (ids.IdentityID, error) {
	var id ids.IdentityID
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, err
	}
	if len(b) != 32 {
		return id, fmt.Errorf("expected 32 bytes, got %d", len(b))
	}
	copy(id[:], b)
	return id, nil
}

// queueSnapshot returns a json-shaped snapshot of every queued entry.
// Holds the queue mutex briefly while reading.
func (s *Subsystem) queueSnapshot() []map[string]any {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	out := make([]map[string]any, 0, s.queue.Len())
	for _, e := range s.queue.entries {
		out = append(out, map[string]any{
			"consumer_id":        hex.EncodeToString(e.ConsumerID[:]),
			"score":              e.CreditScore,
			"enqueued_at":        e.EnqueuedAt.Unix(),
			"effective_priority": e.EffectivePriority(s.nowFn(), s.cfg.AgingAlphaPerMinute),
		})
	}
	return out
}

// drainTopN pops up to n highest-priority entries off the queue and
// returns their consumer-id hex strings.
func (s *Subsystem) drainTopN(n int) []string {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	out := make([]string, 0, n)
	for i := 0; i < n && s.queue.Len() > 0; i++ {
		e := s.queue.Pop()
		out = append(out, hex.EncodeToString(e.ConsumerID[:]))
	}
	return out
}

// ejectQueue removes entries whose hex-encoded RequestID matches reqID.
// Returns the count removed.
func (s *Subsystem) ejectQueue(reqID string) int {
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	keep := s.queue.entries[:0]
	removed := 0
	for _, e := range s.queue.entries {
		if hex.EncodeToString(e.RequestID[:]) == reqID {
			removed++
			continue
		}
		keep = append(keep, e)
	}
	s.queue.entries = keep
	s.queue.Reheapify()
	return removed
}
