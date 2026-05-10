// Package admin is the tracker's operator-facing HTTP server.
//
// The admin server is the single chokepoint for human/automation access to
// tracker internals. It binds the address from config.AdminConfig.ListenAddr
// (default 127.0.0.1:9090, localhost-only by spec §8 — internal port, no
// public exposure). All routes pass through a bearer-token guard before
// reaching their handler; subsystem handlers (broker, admission) mount
// under the same guard so the auth surface is uniform.
//
// Routes:
//
//	GET  /health                       liveness + version
//	GET  /stats                        peer count, ledger tip, merkle interval
//	GET  /peers                        currently-connected QUIC peers
//	POST /peers/add                    501 (federation not yet implemented)
//	POST /peers/remove                 501 (federation not yet implemented)
//	GET  /identity/{id}                registry record + signed balance
//	POST /maintenance                  triggers graceful shutdown
//	*    /broker/...                   mounted from internal/broker
//	*    /admission/...                mounted from internal/admission
//
// Lifecycle: New constructs the Server. Run binds the listener and serves
// until ctx cancels or Shutdown is called. Shutdown drains in-flight
// requests up to its ctx, then force-closes. Both are idempotent: a second
// Run returns ErrAlreadyRunning, a second Shutdown returns nil.
//
// Spec: docs/superpowers/specs/tracker/2026-04-22-tracker-design.md §8, §9.
package admin
