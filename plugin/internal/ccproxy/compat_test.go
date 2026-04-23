package ccproxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsCompatible_HappyPath(t *testing.T) {
	s := &AuthState{LoggedIn: true, AuthMethod: "claude.ai", APIProvider: "firstParty"}
	ok, reason := s.IsCompatible()
	assert.True(t, ok)
	assert.Empty(t, reason)
}

func TestIsCompatible_NotLoggedIn(t *testing.T) {
	s := &AuthState{LoggedIn: false, AuthMethod: "none", APIProvider: "firstParty"}
	ok, reason := s.IsCompatible()
	assert.False(t, ok)
	assert.Equal(t, IncompatNotLoggedIn, reason)
}

func TestIsCompatible_Bedrock(t *testing.T) {
	s := &AuthState{LoggedIn: true, AuthMethod: "claude.ai", APIProvider: "bedrock"}
	ok, reason := s.IsCompatible()
	assert.False(t, ok)
	assert.Equal(t, IncompatWrongProvider, reason)
}

func TestIsCompatible_Vertex(t *testing.T) {
	s := &AuthState{LoggedIn: true, AuthMethod: "claude.ai", APIProvider: "vertex"}
	ok, reason := s.IsCompatible()
	assert.False(t, ok)
	assert.Equal(t, IncompatWrongProvider, reason)
}

func TestIsCompatible_APIKey(t *testing.T) {
	s := &AuthState{LoggedIn: true, AuthMethod: "api_key", APIProvider: "firstParty"}
	ok, reason := s.IsCompatible()
	assert.False(t, ok)
	assert.Equal(t, IncompatWrongMethod, reason)
}
