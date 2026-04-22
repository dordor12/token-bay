package exhaustionproof

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVersionV1_IsOne(t *testing.T) {
	assert.Equal(t, uint8(1), VersionV1)
}

func TestVersionV2_IsTwo(t *testing.T) {
	assert.Equal(t, uint8(2), VersionV2)
}
