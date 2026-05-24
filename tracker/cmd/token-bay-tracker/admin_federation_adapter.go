package main

import (
	"errors"

	"github.com/token-bay/token-bay/shared/ids"
	"github.com/token-bay/token-bay/tracker/internal/admin"
	"github.com/token-bay/token-bay/tracker/internal/federation"
)

// adminFederationAdapter implements admin.FederationView against the
// live *federation.Federation. It is the single place that crosses the
// admin↔federation package boundary: admin imports neither federation
// types nor errors, and federation imports nothing from admin.
type adminFederationAdapter struct {
	fed *federation.Federation
}

func newAdminFederationAdapter(fed *federation.Federation) adminFederationAdapter {
	return adminFederationAdapter{fed: fed}
}

func (a adminFederationAdapter) Peers() []admin.PeerSummary {
	if a.fed == nil {
		return nil
	}
	peers := a.fed.Peers()
	out := make([]admin.PeerSummary, 0, len(peers))
	for _, p := range peers {
		out = append(out, admin.PeerSummary{
			TrackerID:   p.TrackerID,
			PubKey:      append([]byte(nil), p.PubKey...),
			Addr:        p.Addr,
			Region:      p.Region,
			State:       p.State.String(),
			Since:       p.Since,
			HealthScore: a.fed.HealthScore(p.TrackerID),
		})
	}
	return out
}

func (a adminFederationAdapter) AddPeer(req admin.PeerAddRequest) error {
	if a.fed == nil {
		return errors.New("federation not configured")
	}
	err := a.fed.AddPeer(federation.AllowlistedPeer{
		TrackerID: req.TrackerID,
		PubKey:    req.PubKey,
		Addr:      req.Addr,
		Region:    req.Region,
	})
	if errors.Is(err, federation.ErrPeerExists) {
		return admin.ErrPeerExists
	}
	return err
}

func (a adminFederationAdapter) Depeer(id ids.TrackerID) error {
	if a.fed == nil {
		return errors.New("federation not configured")
	}
	err := a.fed.Depeer(id, federation.ReasonOperator)
	if errors.Is(err, federation.ErrPeerUnknown) {
		return admin.ErrPeerUnknown
	}
	return err
}

func (a adminFederationAdapter) ListenAddr() string {
	if a.fed == nil {
		return ""
	}
	return a.fed.ListenAddr()
}

var _ admin.FederationView = adminFederationAdapter{}
