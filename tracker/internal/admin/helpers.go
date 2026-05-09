package admin

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/token-bay/token-bay/shared/ids"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func parseHexID(s string) (ids.IdentityID, error) {
	var id ids.IdentityID
	b, err := hex.DecodeString(s)
	if err != nil {
		return id, err
	}
	if len(b) != 32 {
		return id, fmt.Errorf("expected 32 bytes, got %d", len(b))
	}
	copy(id[:], b)
	return id, nil
}
