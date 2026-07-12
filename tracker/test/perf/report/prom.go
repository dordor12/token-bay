package report

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ParsePromText parses Prometheus text exposition format into a flat
// map of "name" or `name{labels}` -> value. Comment lines and lines
// that don't parse are skipped: the scraper must survive whatever a
// loaded tracker serves, and the report only reads a handful of known
// series. Timestamps (an optional third field) are ignored.
func ParsePromText(r io.Reader) (map[string]float64, error) {
	out := make(map[string]float64)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// The value starts after the last space outside braces; series
		// names/labels never contain spaces outside quoted label values,
		// so splitting on the final space-run is safe for the metrics we
		// read (none carry spaces inside label values).
		lastSpace := strings.LastIndexAny(line, " \t")
		if lastSpace <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:lastSpace])
		fields := strings.Fields(line[lastSpace:])
		if len(fields) == 0 {
			continue
		}
		// key may still end with a timestamp column: "name{...} 12 1699..."
		// splits as key="name{...} 12". Re-split in that case.
		if kf := strings.Fields(key); len(kf) == 2 {
			if v, err := strconv.ParseFloat(kf[1], 64); err == nil {
				out[kf[0]] = v
				continue
			}
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue
		}
		out[key] = v
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("report: scan prometheus text: %w", err)
	}
	return out, nil
}

// SumMetric sums every series of the named metric across label sets.
func SumMetric(m map[string]float64, name string) float64 {
	var sum float64
	for k, v := range m {
		if k == name || strings.HasPrefix(k, name+"{") {
			sum += v
		}
	}
	return sum
}

// MetricLabeled returns the value of one labeled series, e.g.
// MetricLabeled(m, "tokenbay_federation_peers", `state="steady"`).
func MetricLabeled(m map[string]float64, name, labels string) float64 {
	return m[name+"{"+labels+"}"]
}
