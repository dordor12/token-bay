package report

import (
	"sync/atomic"
	"time"
)

// histogram bucket upper bounds. Roughly log-spaced from 1ms to 60s;
// the last implicit bucket is +Inf. Percentile answers are the upper
// bound of the bucket the quantile falls in — good enough for
// threshold floors (spec §9: catch collapses, not drift).
var bucketBounds = [...]time.Duration{
	1 * time.Millisecond,
	2 * time.Millisecond,
	5 * time.Millisecond,
	10 * time.Millisecond,
	20 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

// Histogram is a lock-free fixed-bucket latency histogram safe for
// concurrent Observe from many simulated clients. The zero value is
// ready to use.
type Histogram struct {
	buckets [len(bucketBounds) + 1]atomic.Int64
	count   atomic.Int64
}

// Observe records one latency sample.
func (h *Histogram) Observe(d time.Duration) {
	i := 0
	for ; i < len(bucketBounds); i++ {
		if d <= bucketBounds[i] {
			break
		}
	}
	h.buckets[i].Add(1)
	h.count.Add(1)
}

// Count returns the number of samples observed.
func (h *Histogram) Count() int64 { return h.count.Load() }

// Percentile returns an upper bound for the q-quantile (q in [0, 1]).
// Zero samples => 0. A quantile landing in the overflow bucket returns
// slightly above the last bound.
func (h *Histogram) Percentile(q float64) time.Duration {
	total := h.count.Load()
	if total == 0 {
		return 0
	}
	rank := int64(q * float64(total))
	if rank < 1 {
		rank = 1
	}
	if rank > total {
		rank = total
	}
	var seen int64
	for i := range h.buckets {
		seen += h.buckets[i].Load()
		if seen >= rank {
			if i < len(bucketBounds) {
				return bucketBounds[i]
			}
			return bucketBounds[len(bucketBounds)-1] + time.Second
		}
	}
	return bucketBounds[len(bucketBounds)-1] + time.Second
}
