package proto

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestProtocolVersion_IsOne(t *testing.T) {
	assert.Equal(t, uint16(1), ProtocolVersion)
}
