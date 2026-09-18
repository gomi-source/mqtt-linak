package bridge

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gomi-source/corebluetooth-go/ble"

	"github.com/gomi-source/mqtt-linak/internal/config"
)

// ---------------------------------------------------------------------
// Payload parsing
// ---------------------------------------------------------------------

func TestParseTenthsMM(t *testing.T) {
	tests := []struct {
		payload string
		want    int
		wantErr string
	}{
		{"7350", 7350, ""},
		{"  7350\n", 7350, ""},
		{`"7350"`, 7350, ""},
		{"0", 0, ""},
		{"65535", 65535, ""},
		{"735.4", 735, ""}, // rounded, not rejected
		{"735.6", 736, ""}, // rounded to nearest
		{`{"value": 7350}`, 7350, ""},
		{`{"height": 7350}`, 7350, ""},
		{`{"position": "7350"}`, 7350, ""},
		{`{"base_height": 620}`, 620, ""},
		{"", 0, "empty payload"},
		{"up", 0, "not a number"},
		{"-1", 0, "outside the desk's range"},
		{"65536", 0, "outside the desk's range"},
		{`{"speed": 5}`, 0, "none of the fields"},
		{`{"value": "fast"}`, 0, "not a number"},
		{"{not json", 0, "neither a number nor a JSON object"},
	}

	for _, tc := range tests {
		t.Run(tc.payload, func(t *testing.T) {
			got, err := parseTenthsMM([]byte(tc.payload))
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("want an error containing %q, got %d", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// The control service is only visible after connecting, so filtering a
// scan on it would mean never discovering a desk at all.
func TestDefaultScanIsUnfiltered(t *testing.T) {
	if len(DefaultScanServiceUUIDs) != 0 {
		t.Fatalf("DefaultScanServiceUUIDs = %v, want empty: a LINAK controller does not advertise its services",
			DefaultScanServiceUUIDs)
	}
}

func TestTopicBuilding(t *testing.T) {
	if got := commandTopic("cmd/linak", "office", leafHeight); got != "cmd/linak/office/height" {
		t.Errorf("commandTopic = %q", got)
	}
	if got := metricTopic("tele/linak", "office", leafBaseHeight); got != "tele/linak/office/base_height" {
		t.Errorf("metricTopic = %q", got)
	}
	if got := metricTopic("tele/linak", config.ReservedDeskID, leafAvailability); got != "tele/linak/bridge/availability" {
		t.Errorf("bridge availability topic = %q", got)
	}
}

// ---------------------------------------------------------------------
// Desk matching
// ---------------------------------------------------------------------

func strptr(s string) *string { return &s }

func TestMatchesDesk(t *testing.T) {
	const pid = "1234ABCD-0000-0000-0000-000000000000"

	byName := config.Desk{ID: "office", Name: "Desk 8802"}
	byPrefix := config.Desk{ID: "office", NamePrefix: "desk"}
	byID := config.Desk{ID: "office", PeripheralID: strings.ToLower(pid)}

	tests := []struct {
		name string
		cfg  config.Desk
		p    ble.DiscoveredPeripheral
		want bool
	}{
		{
			"peripheral name matches case-insensitively",
			byName,
			ble.DiscoveredPeripheral{PeripheralID: pid, Name: strptr("desk 8802")},
			true,
		},
		{
			"advertised local name matches when the peripheral name is absent",
			byName,
			ble.DiscoveredPeripheral{PeripheralID: pid, AdvertisementData: ble.AdvertisementData{LocalName: strptr("Desk 8802")}},
			true,
		},
		{
			"different name does not match",
			byName,
			ble.DiscoveredPeripheral{PeripheralID: pid, Name: strptr("Desk 1111")},
			false,
		},
		{
			"peripheral id matches regardless of case",
			byID,
			ble.DiscoveredPeripheral{PeripheralID: pid, Name: strptr("something else")},
			true,
		},
		{
			"peripheral id takes precedence over a matching name",
			byID,
			ble.DiscoveredPeripheral{PeripheralID: "OTHER-ID", Name: strptr("Desk 8802")},
			false,
		},
		{
			"nameless peripheral does not match a name rule",
			byName,
			ble.DiscoveredPeripheral{PeripheralID: pid},
			false,
		},
		{
			"name_prefix matches a longer name",
			byPrefix,
			ble.DiscoveredPeripheral{PeripheralID: pid, Name: strptr("Desk 8802")},
			true,
		},
		{
			"name_prefix matches an exactly-as-long name",
			byPrefix,
			ble.DiscoveredPeripheral{PeripheralID: pid, Name: strptr("DESK")},
			true,
		},
		{
			"name_prefix does not match a shorter name",
			byPrefix,
			ble.DiscoveredPeripheral{PeripheralID: pid, Name: strptr("De")},
			false,
		},
		{
			"name_prefix does not match elsewhere in the name",
			byPrefix,
			ble.DiscoveredPeripheral{PeripheralID: pid, Name: strptr("My Desk")},
			false,
		},
		{
			"name matches the advertised local name when the peripheral name differs",
			byName,
			ble.DiscoveredPeripheral{
				PeripheralID:      pid,
				Name:              strptr("LINAK"),
				AdvertisementData: ble.AdvertisementData{LocalName: strptr("Desk 8802")},
			},
			true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesDesk(tc.cfg, tc.p); got != tc.want {
				t.Fatalf("matchesDesk = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------
// Throttle
// ---------------------------------------------------------------------

type recorder struct {
	mu   sync.Mutex
	vals []int
}

func (r *recorder) add(v int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vals = append(r.vals, v)
}

func (r *recorder) snapshot() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.vals...)
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fakeClock lets the throttle tests run without sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestThrottle(min time.Duration) (*throttle, *recorder, *fakeClock) {
	rec := &recorder{}
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	th := newThrottle(min, rec.add)
	th.now = clk.now
	return th, rec, clk
}

func TestThrottlePublishesFirstValueImmediately(t *testing.T) {
	th, rec, _ := newTestThrottle(time.Second)
	th.Set(1000, false)
	if got := rec.snapshot(); !equal(got, []int{1000}) {
		t.Fatalf("got %v, want [1000]", got)
	}
}

func TestThrottleCoalescesWithinTheInterval(t *testing.T) {
	th, rec, clk := newTestThrottle(time.Second)
	th.Set(1000, false) // published
	clk.advance(100 * time.Millisecond)
	th.Set(1100, false) // pending
	th.Set(1200, false) // replaces pending
	if got := rec.snapshot(); !equal(got, []int{1000}) {
		t.Fatalf("got %v, want only the first value", got)
	}

	// Flushing too early keeps it pending.
	th.Flush()
	if got := rec.snapshot(); !equal(got, []int{1000}) {
		t.Fatalf("got %v, an early flush should publish nothing", got)
	}

	clk.advance(time.Second)
	th.Flush()
	if got := rec.snapshot(); !equal(got, []int{1000, 1200}) {
		t.Fatalf("got %v, want the latest pending value after the interval", got)
	}
}

func TestThrottleForcePublishesImmediately(t *testing.T) {
	th, rec, clk := newTestThrottle(time.Second)
	th.Set(1000, false)
	clk.advance(10 * time.Millisecond)
	th.Set(1500, true) // desk reported speed 0
	if got := rec.snapshot(); !equal(got, []int{1000, 1500}) {
		t.Fatalf("got %v, want the forced value published at once", got)
	}
}

func TestThrottleDropsUnchangedValues(t *testing.T) {
	th, rec, clk := newTestThrottle(time.Second)
	th.Set(1000, false)
	clk.advance(2 * time.Second)
	th.Set(1000, false)
	th.Set(1000, true)
	if got := rec.snapshot(); !equal(got, []int{1000}) {
		t.Fatalf("got %v, want repeats dropped", got)
	}
}

func TestThrottleResetForcesRepublish(t *testing.T) {
	th, rec, clk := newTestThrottle(time.Second)
	th.Set(1000, false)
	th.Reset() // e.g. after a reconnect
	clk.advance(10 * time.Millisecond)
	th.Set(1000, false)
	if got := rec.snapshot(); !equal(got, []int{1000, 1000}) {
		t.Fatalf("got %v, want the value republished after Reset", got)
	}
}

func TestThrottleFlushWithNothingPending(t *testing.T) {
	th, rec, _ := newTestThrottle(time.Second)
	th.Flush()
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("got %v, want nothing published", got)
	}
}
