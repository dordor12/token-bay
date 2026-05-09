package admin

import (
	"net/http"
	"strings"
)

// MuxGuard wraps a route handler with auth (and any future cross-cutting
// concerns). The signature matches admission.MuxGuard so the admission
// subsystem can register through the same guard the admin Server uses.
type MuxGuard func(http.Handler) http.Handler

// TokenValidator returns true if the supplied bearer token authorizes the
// request. The validator is supplied by composition (cmd/run_cmd reads
// TOKEN_BAY_ADMIN_TOKEN from the environment); tests inject a constant
// validator. A nil validator rejects every request.
type TokenValidator func(token string) bool

// bearerGuard returns middleware that enforces "Authorization: Bearer X".
// The realm value is included in WWW-Authenticate so curl/clients see
// which surface the 401 came from.
func bearerGuard(validate TokenValidator) MuxGuard {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			const prefix = "Bearer "
			hdr := r.Header.Get("Authorization")
			if validate == nil || !strings.HasPrefix(hdr, prefix) ||
				!validate(strings.TrimPrefix(hdr, prefix)) {
				w.Header().Set("WWW-Authenticate", `Bearer realm="tracker-admin"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
