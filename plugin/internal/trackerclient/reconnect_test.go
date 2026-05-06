package trackerclient

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBackoffDelayBounded(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	base := 100 * time.Millisecond
	maxDelay := 5 * time.Second
	for attempt := 0; attempt < 20; attempt++ {
		d := backoffDelay(attempt, base, maxDelay, r)
		assert.LessOrEqual(t, d, maxDelay, "attempt=%d", attempt)
		assert.GreaterOrEqual(t, d, base/2, "attempt=%d", attempt)
	}
}

func TestBackoffDelayMonotonicEnvelope(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	base := 100 * time.Millisecond
	maxDelay := 30 * time.Second
	saturated := backoffDelay(10, base, maxDelay, r)
	assert.GreaterOrEqual(t, saturated, maxDelay/2)
}
