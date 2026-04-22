package ids

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIdentityID_ZeroValue_IsZeroBytes(t *testing.T) {
	var id IdentityID
	assert.Equal(t, [32]byte{}, id.Bytes())
}
