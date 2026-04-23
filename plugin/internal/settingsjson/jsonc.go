package settingsjson

import (
	"bytes"

	"github.com/tailscale/hujson"
)

// hasJSONCComments reports whether data contains JSONC-isms (comments or
// trailing commas) that a round-trip through encoding/json would lose.
//
// Detection strategy: hujson.Standardize replaces JSONC extensions with
// whitespace of the same byte-length (comments become spaces; trailing
// commas become spaces). So any byte-level difference between the original
// and the standardized form means the input had JSONC content.
//
// Note: hujson.Standardize mutates its input slice in place. We must
// compare against a copy taken before the call.
//
// Returns an error only if the input is not even valid JSONC (malformed).
func hasJSONCComments(data []byte) (bool, error) {
	original := make([]byte, len(data))
	copy(original, data)
	standardized, err := hujson.Standardize(data)
	if err != nil {
		return false, err
	}
	return !bytes.Equal(original, standardized), nil
}
