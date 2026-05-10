// Package bootstrap implements the plugin-side of federation §7.2 plugin
// bootstrap distribution: parsing a signed BootstrapPeerList file into the
// trackerclient endpoint shape, maintaining a local known_peers cache, and
// persisting the latest signed snapshot back to bootstrap.signed.
//
// This package is consumed by the sidecar's run command to resolve
// `tracker = "auto"` config into an actual list of TrackerEndpoints.
package bootstrap
