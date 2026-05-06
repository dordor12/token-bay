package broker

import "github.com/token-bay/token-bay/tracker/internal/config"

// Subsystems is the composite that ties *Broker and *Settlement to a shared
// in-memory state (Inflight + Reservations). Construct via Open; the two
// subsystems are accessible as fields for api/ wiring.
type Subsystems struct {
	Broker     *Broker
	Settlement *Settlement
}

// Open constructs both subsystems sharing one Inflight + one Reservations.
// Goroutine ownership is split: queue-drain lives with Broker, reservation
// TTL reaper lives with Settlement. Close() shuts both down in dependency
// order.
func Open(cfg config.BrokerConfig, scfg config.SettlementConfig, deps Deps) (*Subsystems, error) {
	inflt := NewInflight()
	resv := NewReservations()
	b, err := OpenBroker(cfg, scfg, deps, inflt, resv)
	if err != nil {
		return nil, err
	}
	s, err := OpenSettlement(scfg, deps, inflt, resv)
	if err != nil {
		_ = b.Close()
		return nil, err
	}
	return &Subsystems{Broker: b, Settlement: s}, nil
}

// Close shuts down both subsystems in dependency order. Idempotent.
func (s *Subsystems) Close() error {
	err1 := s.Broker.Close()
	err2 := s.Settlement.Close()
	if err1 != nil {
		return err1
	}
	return err2
}
