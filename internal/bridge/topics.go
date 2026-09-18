package bridge

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Topic leaves. Commands and metrics deliberately use the same leaf names
// under different bases, so a controller can mirror what it reads.
const (
	leafHeight       = "height"
	leafBaseHeight   = "base_height"
	leafAvailability = "availability"
)

// commandTopic builds {base}/{deskID}/{leaf}.
func commandTopic(base, deskID, leaf string) string {
	return strings.Join([]string{base, deskID, leaf}, "/")
}

// metricTopic builds {base}/{deskID}/{leaf}.
func metricTopic(base, deskID, leaf string) string {
	return strings.Join([]string{base, deskID, leaf}, "/")
}

// maxTenthsMM is the largest value the desk's 16-bit height fields can
// carry. Everything on the wire to the desk is little-endian uint16 tenths
// of a millimetre, so 65535 is 6553.5 mm.
const maxTenthsMM = 65535

// parseTenthsMM reads a command payload into a height in tenths of a
// millimetre.
//
// The plain form is just the number ("7350"). JSON is also accepted so a
// home-automation system that only speaks JSON templates can publish
// {"value": 7350} (or "height"/"position") without extra templating. A
// fractional value is rounded to the nearest tenth of a millimetre rather
// than rejected, since "735.0" is a natural thing for a templating engine
// to emit.
func parseTenthsMM(payload []byte) (int, error) {
	s := strings.TrimSpace(string(payload))
	if s == "" {
		return 0, fmt.Errorf("empty payload")
	}

	if v, err := parseNumber(s); err == nil {
		return checkRange(v)
	}

	if strings.HasPrefix(s, "{") {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(s), &obj); err != nil {
			return 0, fmt.Errorf("payload %q is neither a number nor a JSON object: %w", s, err)
		}
		for _, key := range []string{"value", "height", "position", "base_height"} {
			raw, ok := obj[key]
			if !ok {
				continue
			}
			inner := strings.Trim(strings.TrimSpace(string(raw)), `"`)
			v, err := parseNumber(inner)
			if err != nil {
				return 0, fmt.Errorf("JSON field %q is not a number: %q", key, inner)
			}
			return checkRange(v)
		}
		return 0, fmt.Errorf("JSON payload has none of the fields value, height, position, base_height")
	}

	return 0, fmt.Errorf("payload %q is not a number", s)
}

func parseNumber(s string) (float64, error) {
	s = strings.Trim(s, `"'`)
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	return strconv.ParseFloat(s, 64)
}

func checkRange(v float64) (int, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("value is not finite")
	}
	n := int(math.Round(v))
	if n < 0 || n > maxTenthsMM {
		return 0, fmt.Errorf("value %d is outside the desk's range 0..%d tenths of a millimetre", n, maxTenthsMM)
	}
	return n, nil
}

// formatTenthsMM renders a metric payload. Plain integers keep the topics
// usable from a shell with mosquitto_sub.
func formatTenthsMM(v int) string { return strconv.Itoa(v) }
