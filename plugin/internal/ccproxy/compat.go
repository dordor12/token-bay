package ccproxy

// Human-readable reasons returned by AuthState.IsCompatible when the
// probed auth state precludes Token-Bay redirect.
const (
	IncompatNotLoggedIn   = "user is not logged into Claude Code (run: claude auth login)"
	IncompatWrongProvider = "apiProvider is not 'firstParty' — Token-Bay cannot redirect under Bedrock/Vertex/Foundry"
	IncompatWrongMethod   = "authMethod is not 'claude.ai' — /usage probe requires OAuth-based Claude AI subscription auth"
)

// IsCompatible reports whether this AuthState permits Token-Bay mid-session
// redirect. The gate requires a logged-in first-party claude.ai subscription
// because the /usage probe and orgId identity binding both depend on it.
func (a *AuthState) IsCompatible() (bool, string) {
	if !a.LoggedIn {
		return false, IncompatNotLoggedIn
	}
	if a.APIProvider != "firstParty" {
		return false, IncompatWrongProvider
	}
	if a.AuthMethod != "claude.ai" {
		return false, IncompatWrongMethod
	}
	return true, ""
}
