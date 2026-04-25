// Package entry owns the operations on shared/proto.Entry —
// canonical-bytes hashing, builder helpers per kind, and signature
// verification. The Entry type itself lives in shared/proto/ because it
// is a wire-format type federation streams between trackers.
//
// Spec: docs/superpowers/specs/ledger/2026-04-22-ledger-design.md §3.1.
//
// Non-negotiables:
//
//  1. Never sign the wire form (Entry); always sign EntryBody. The hash
//     and all three signatures are over DeterministicMarshal(body).
//  2. The entry hash is over the body, not the signed form. This keeps
//     the chain stable across tracker key rotations that re-sign the tip.
//  3. Builders return validated bodies. Callers can sign without
//     re-running ValidateEntryBody.
package entry
