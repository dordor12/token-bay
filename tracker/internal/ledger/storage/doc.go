// Package storage owns the SQLite-backed durable layer for the
// tracker's append-only ledger.
//
// Spec: docs/superpowers/specs/ledger/2026-04-22-ledger-design.md
//
//	§3 (data model), §4.1 (append algorithm), §5 (storage backend).
//
// Non-negotiables:
//
//  1. AppendEntry is atomic across all touched tables — entries,
//     balances, and (when applicable) merkle_roots — within a single
//     SQLite transaction.
//  2. The chain is append-only. There are no UPDATE or DELETE statements
//     against entries or merkle_roots. Balances are projections and
//     CAN be updated; the chain replay is the source of truth.
//  3. Storage trusts its caller. It validates row shape (lengths,
//     non-null) but does NOT verify hashes, signatures, balance math,
//     or chain linkage. The orchestrator (internal/ledger) does.
//  4. Writes are serialized through Store.writeMu. Reads do not take
//     the mutex — SQLite WAL allows concurrent readers.
package storage
