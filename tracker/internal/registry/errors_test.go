package registry

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSentinelErrors_NotNil(t *testing.T) {
	assert.NotNil(t, ErrUnknownSeeder)
	assert.NotNil(t, ErrInvalidShardCount)
	assert.NotNil(t, ErrInvalidHeadroom)
	assert.NotNil(t, ErrLoadUnderflow)
}

func TestSentinelErrors_Distinct(t *testing.T) {
	all := []error{
		ErrUnknownSeeder,
		ErrInvalidShardCount,
		ErrInvalidHeadroom,
		ErrLoadUnderflow,
	}
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			assert.False(t, errors.Is(all[i], all[j]),
				"errors %v and %v should be distinct sentinels", all[i], all[j])
		}
	}
}

func TestSentinelErrors_HumanReadable(t *testing.T) {
	assert.Contains(t, ErrUnknownSeeder.Error(), "unknown seeder")
	assert.Contains(t, ErrInvalidShardCount.Error(), "shard count")
	assert.Contains(t, ErrInvalidHeadroom.Error(), "headroom")
	assert.Contains(t, ErrLoadUnderflow.Error(), "load")
}
