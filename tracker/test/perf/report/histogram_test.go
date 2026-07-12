package report

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestHistogram_EmptyPercentileIsZero(t *testing.T) {
	var h Histogram
	assert.Equal(t, time.Duration(0), h.Percentile(0.99))
	assert.Equal(t, int64(0), h.Count())
}

func TestHistogram_PercentilesAreUpperBounds(t *testing.T) {
	var h Histogram
	// 99 observations at ~10ms, one at ~2s.
	for range 99 {
		h.Observe(10 * time.Millisecond)
	}
	h.Observe(2 * time.Second)

	assert.Equal(t, int64(100), h.Count())

	p50 := h.Percentile(0.50)
	assert.GreaterOrEqual(t, p50, 10*time.Millisecond)
	assert.Less(t, p50, 100*time.Millisecond, "p50 should land in a small bucket")

	p99 := h.Percentile(0.99)
	assert.Less(t, p99, 2*time.Second, "p99 excludes the single outlier")

	p100 := h.Percentile(1.0)
	assert.GreaterOrEqual(t, p100, 2*time.Second, "p100 covers the outlier's bucket")
}

func TestHistogram_ConcurrentObserve(t *testing.T) {
	var h Histogram
	done := make(chan struct{})
	for range 8 {
		go func() {
			for range 1000 {
				h.Observe(5 * time.Millisecond)
			}
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-done
	}
	assert.Equal(t, int64(8000), h.Count())
}
