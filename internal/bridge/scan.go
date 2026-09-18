package bridge

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/gomi-source/corebluetooth-go/ble"

	"github.com/gomi-source/mqtt-linak/internal/config"
)

// Sighting is one peripheral seen during an inventory scan.
type Sighting struct {
	PeripheralID string
	Name         string
	RSSI         int
	Services     []string
	Manufacturer string
	Connectable  bool
}

// Inventory scans for the given duration and reports everything seen, so
// the desk's name or peripheral ID can be copied into the config file.
//
// This exists because a LINAK controller does not advertise its control
// service, so there is no service UUID to filter on and no way to tell a
// desk from a pair of headphones without looking at the list.
func Inventory(ctx context.Context, bt config.Bluetooth, d time.Duration, log *slog.Logger) ([]Sighting, error) {
	client, err := ble.Start(ctx, ble.Options{
		HelperPath: bt.HelperPath,
		Stderr:     newLineWriter(log, "corebluetoothd"),
	})
	if err != nil {
		return nil, fmt.Errorf("starting CoreBluetooth helper: %w", err)
	}
	defer client.Close()

	if state, err := client.State(ctx); err == nil && state != ble.StatePoweredOn {
		return nil, fmt.Errorf("bluetooth adapter is %s, not poweredOn "+
			"(check System Settings > Privacy & Security > Bluetooth)", state)
	}

	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = client.StartScan(sctx, ble.ScanOptions{ServiceUUIDs: bt.ScanServiceUUIDs})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("starting scan: %w", err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = client.StopScan(c)
		cancel()
	}()

	found := map[string]Sighting{}
	deadline := time.After(d)
	for {
		select {
		case <-ctx.Done():
			return sorted(found), ctx.Err()
		case <-deadline:
			return sorted(found), nil
		case p, ok := <-client.Discoveries():
			if !ok {
				return sorted(found), nil
			}
			key := strings.ToUpper(p.PeripheralID)
			s := Sighting{
				PeripheralID: p.PeripheralID,
				Name:         peripheralName(p),
				RSSI:         p.RSSI,
				Services:     p.AdvertisementData.ServiceUUIDs,
				Manufacturer: manufacturerHex(p),
			}
			if p.IsConnectable != nil {
				s.Connectable = *p.IsConnectable
			}
			// Later advertisements often carry more than the first, so
			// keep whichever sighting names the device.
			if prev, seen := found[key]; !seen || (prev.Name == "" && s.Name != "") {
				found[key] = s
			}
		}
	}
}

func manufacturerHex(p ble.DiscoveredPeripheral) string {
	if p.AdvertisementData.ManufacturerDataBase64 == nil {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(*p.AdvertisementData.ManufacturerDataBase64)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for i, b := range raw {
		if i > 0 {
			sb.WriteByte(' ')
		}
		fmt.Fprintf(&sb, "%02X", b)
	}
	return sb.String()
}

func sorted(m map[string]Sighting) []Sighting {
	out := make([]Sighting, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	// Named devices first, then by signal strength: the desk is usually
	// the strongest named thing in the room.
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Name != "") != (out[j].Name != "") {
			return out[i].Name != ""
		}
		return out[i].RSSI > out[j].RSSI
	})
	return out
}

// PrintInventory renders the result of Inventory as a table.
func PrintInventory(w io.Writer, sightings []Sighting) {
	if len(sightings) == 0 {
		fmt.Fprintln(w, "Nothing found. If the desk's display is asleep, press a button on the")
		fmt.Fprintln(w, "panel to wake it and scan again.")
		return
	}

	fmt.Fprintf(w, "%-38s  %-24s  %5s  %s\n", "PERIPHERAL ID", "NAME", "RSSI", "ADVERTISED SERVICES")
	for _, s := range sightings {
		name := s.Name
		if name == "" {
			name = "-"
		}
		services := strings.Join(s.Services, " ")
		if services == "" {
			services = "-"
		}
		fmt.Fprintf(w, "%-38s  %-24s  %5d  %s\n", s.PeripheralID, name, s.RSSI, services)
		if s.Manufacturer != "" {
			fmt.Fprintf(w, "%-38s  manufacturer data: %s\n", "", s.Manufacturer)
		}
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Copy the desk's name into a desks entry, or pin it by peripheral_id:")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  desks:")
	fmt.Fprintf(w, "    - id: mydesk\n      peripheral_id: \"%s\"\n", sightings[0].PeripheralID)
}
