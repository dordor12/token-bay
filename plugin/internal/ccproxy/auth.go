package ccproxy

// AuthState mirrors the JSON output of `claude auth status --json`.
// See src/cli/handlers/auth.ts:293-316 upstream for the authoritative
// field list.
type AuthState struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	APIProvider      string `json:"apiProvider"`
	APIKeySource     string `json:"apiKeySource,omitempty"`
	Email            string `json:"email,omitempty"`
	OrgID            string `json:"orgId,omitempty"`
	OrgName          string `json:"orgName,omitempty"`
	SubscriptionType string `json:"subscriptionType,omitempty"`
}
