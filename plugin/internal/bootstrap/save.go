package bootstrap

import "errors"

// SaveSignedBootstrapList atomically writes the marshaled BootstrapPeerList
// bytes to path (write-temp + rename). The intended consumer is the
// sidecar's "fresh-snapshot from FetchBootstrapPeers → bootstrap.signed
// cache" loop: keep the latest tracker-issued list on disk so the next
// startup's auto-bootstrap has fresh endpoints.
//
// The bytes must be a marshaled BootstrapPeerList — this helper does
// not re-validate the signature. Callers obtain trusted bytes from a
// verified RPC response.
func SaveSignedBootstrapList(path string, signedBytes []byte) error {
	if len(signedBytes) == 0 {
		return errors.New("bootstrap: SaveSignedBootstrapList: empty bytes")
	}
	return atomicWriteFile(path, signedBytes, 0o600)
}
