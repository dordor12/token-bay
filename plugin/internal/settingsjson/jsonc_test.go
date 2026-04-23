package settingsjson

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHasJSONCComments_PlainJSON_ReturnsFalse(t *testing.T) {
	input := []byte(`{"env": {"ANTHROPIC_BASE_URL": "https://api.anthropic.com"}}`)
	has, err := hasJSONCComments(input)
	assert.NoError(t, err)
	assert.False(t, has)
}

func TestHasJSONCComments_LineComment_ReturnsTrue(t *testing.T) {
	input := []byte(`{
  // my Anthropic proxy
  "env": {"ANTHROPIC_BASE_URL": "https://api.anthropic.com"}
}`)
	has, err := hasJSONCComments(input)
	assert.NoError(t, err)
	assert.True(t, has)
}

func TestHasJSONCComments_BlockComment_ReturnsTrue(t *testing.T) {
	input := []byte(`{
  /* set by IT */
  "env": {"ANTHROPIC_BASE_URL": "https://api.anthropic.com"}
}`)
	has, err := hasJSONCComments(input)
	assert.NoError(t, err)
	assert.True(t, has)
}

func TestHasJSONCComments_TrailingComma_ReturnsTrue(t *testing.T) {
	// JSONC also allows trailing commas; if present, treat as "non-preservable".
	input := []byte(`{
  "env": {"ANTHROPIC_BASE_URL": "https://api.anthropic.com",}
}`)
	has, err := hasJSONCComments(input)
	assert.NoError(t, err)
	assert.True(t, has)
}

func TestHasJSONCComments_MalformedInput_ReturnsError(t *testing.T) {
	input := []byte(`{this is not json`)
	_, err := hasJSONCComments(input)
	assert.Error(t, err)
}
