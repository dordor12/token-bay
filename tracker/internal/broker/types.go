package broker

import "github.com/token-bay/token-bay/shared/ids"

// Outcome enumerates Submit's possible result shapes.
type Outcome uint8

const (
	OutcomeUnspecified Outcome = iota
	OutcomeAdmit
	OutcomeQueued
	OutcomeRejected
	OutcomeNoCapacity
)

// Assignment is the body of an Admit outcome — what the consumer needs to
// open the seeder↔consumer tunnel.
type Assignment struct {
	SeederAddr       string
	SeederPubkey     []byte
	ReservationToken []byte // 16 bytes
	RequestID        [16]byte
	AssignedSeeder   ids.IdentityID
}

type NoCapacityDetails struct {
	Reason string
}

// QueuedDetails / RejectedDetails are emitted by the api/-layer admission
// gate. broker.Submit itself never returns OutcomeQueued / OutcomeRejected —
// they live here so the api layer can express the four wire variants
// uniformly via a single broker.Result.
type QueuedDetails struct {
	RequestID    [16]byte
	PositionBand uint8
	EtaBand      uint8
}

type RejectedDetails struct {
	Reason      uint8
	RetryAfterS uint32
}

type Result struct {
	Outcome  Outcome
	Admit    *Assignment
	NoCap    *NoCapacityDetails
	Queued   *QueuedDetails
	Rejected *RejectedDetails
}
