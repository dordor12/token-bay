package ledger

import (
	"fmt"
	"math"
	"time"
)

// unixSecondsU returns t.Unix() as uint64, saturating at 0 for pre-1970
// inputs. Avoids gosec G115's int64→uint64 narrowing complaint without
// adding spurious error paths for clocks that will never be negative.
func unixSecondsU(t time.Time) uint64 {
	s := t.Unix()
	if s < 0 {
		return 0
	}
	return uint64(s)
}

// signedAmount converts an unsigned credit amount to int64 with an
// explicit bounds check. Public Append* methods already validate amounts
// for zero/non-zero; this layer rejects amounts that exceed MaxInt64,
// which would silently overflow on the cast and is also what gosec G115
// flags.
func signedAmount(amount uint64) (int64, error) {
	if amount > math.MaxInt64 {
		return 0, fmt.Errorf("ledger: amount %d exceeds int64 range", amount)
	}
	return int64(amount), nil
}
