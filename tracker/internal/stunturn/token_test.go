package stunturn

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestToken_String_Hex(t *testing.T) {
	tok := Token{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	}

	assert.Equal(t, "0102030405060708090a0b0c0d0e0f10", tok.String())
}

func TestToken_Zero_String(t *testing.T) {
	var tok Token

	assert.Equal(t, "00000000000000000000000000000000", tok.String())
}

// TestToken_UsableAsMapKey ensures Token can key a Go map (it is a value
// array; this is only a smoke test to catch accidental future changes
// like switching to a slice).
func TestToken_UsableAsMapKey(t *testing.T) {
	m := map[Token]int{}
	m[Token{1}] = 1
	m[Token{2}] = 2

	assert.Equal(t, 2, len(m))
	assert.Equal(t, 1, m[Token{1}])
}
