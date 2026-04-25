// Package ledger is the chain orchestrator for the tracker's append-only
// credit ledger. It wires together internal/ledger/entry (build, hash,
// verify), internal/ledger/storage (durable layer), the tracker keypair,
// and a clock into a chain-integrity-preserving API.
//
// Spec: docs/superpowers/specs/ledger/2026-04-22-ledger-design.md §4.
//
// Non-negotiables:
//
//  1. The Ledger.mu mutex covers the entire append critical section
//     (tip-read → build → sign → storage write). It IS the spec's
//     "ledger write lock" from §4.1 step 1. Append never holds the lock
//     across a network round-trip — the caller pre-collects counterparty
//     sigs, the orchestrator verifies them against the current tip in
//     one atomic step, returning ErrStaleTip on mismatch so the caller
//     can retry against a fresh tip.
//  2. Reads (Tip, EntryBy*, EntriesSince, SignedBalance) do not take
//     the append lock. SignedBalance's internal consistency comes from
//     storage.BalanceWithTip's single read transaction.
//  3. Stale-tip mismatches return ErrStaleTip. The orchestrator does
//     NOT retry — the broker layer above does (except IssueStarterGrant,
//     which retries internally because rebuilding is cheap and the grant
//     is tracker-only-signed).
//  4. Balance arithmetic happens here, not in storage. Storage trusts
//     the orchestrator to pass already-computed BalanceUpdate values.
package ledger
