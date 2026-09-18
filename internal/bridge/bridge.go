// Package bridge wires LINAK desk controllers reached over CoreBluetooth
// to an MQTT broker.
//
// One Bridge owns a single corebluetoothd helper (one CBCentralManager per
// process is a CoreBluetooth constraint, and corebluetooth-go spawns one
// helper per client), one MQTT connection, and one deskManager per
// configured desk. The BLE client delivers discoveries, disconnections,
// adapter state changes and errors on four shared channels; a single event
// pump goroutine reads all four and fans them out to the desk managers, so
// nothing else in this package touches those channels.
package bridge

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gomi-source/corebluetooth-go/ble"

	"github.com/gomi-source/mqtt-linak/internal/config"
	"github.com/gomi-source/mqtt-linak/internal/mqttc"
)

// DefaultScanServiceUUIDs is empty on purpose: scan for everything.
//
// It is tempting to filter on the LINAK control service
// (99FA0001-338A-1024-8A49-009C0215F78A), but a DPG controller does not
// put it in its advertisement - that service only becomes visible after
// connecting and discovering. CoreBluetooth matches scan filters against
// the advertisement alone, so filtering on it means never finding a desk.
//
// bluetooth.scan_service_uuids can still narrow the scan to whatever a
// particular controller does advertise; `mqtt-linak -scan` prints the
// advertised services of everything nearby.
var DefaultScanServiceUUIDs = []string{}

// Bridge is the whole application: BLE on one side, MQTT on the other.
type Bridge struct {
	cfg *config.Config
	log *slog.Logger

	bleClient *ble.Client
	mq        atomic.Pointer[mqttc.Client]

	desks []*deskManager
	// byPeripheral maps an upper-cased peripheral ID to the desk manager
	// that owns it, for routing disconnect events.
	byPeripheral sync.Map
	// seen records peripheral IDs already logged, so debug logging names
	// each nearby device once instead of once per advertisement.
	seen sync.Map

	scanWake chan struct{}
}

// New builds a bridge from a validated config.
func New(cfg *config.Config, log *slog.Logger) *Bridge {
	if log == nil {
		log = slog.Default()
	}
	b := &Bridge{cfg: cfg, log: log, scanWake: make(chan struct{}, 1)}
	for i := range cfg.Desks {
		b.desks = append(b.desks, newDeskManager(b, cfg.Desks[i]))
	}
	return b
}

// Run starts everything and blocks until ctx is cancelled, then shuts the
// BLE helper and MQTT connection down cleanly.
func (b *Bridge) Run(ctx context.Context) error {
	scanUUIDs := b.cfg.Bluetooth.ScanServiceUUIDs
	if scanUUIDs == nil {
		scanUUIDs = DefaultScanServiceUUIDs
	}

	bleClient, err := ble.Start(ctx, ble.Options{
		HelperPath: b.cfg.Bluetooth.HelperPath,
		Stderr:     newLineWriter(b.log, "corebluetoothd"),
	})
	if err != nil {
		return fmt.Errorf("starting CoreBluetooth helper: %w", err)
	}
	b.bleClient = bleClient
	defer func() {
		if cerr := bleClient.Close(); cerr != nil {
			b.log.Debug("closing CoreBluetooth helper", "err", cerr)
		}
	}()

	if state, err := bleClient.State(ctx); err == nil {
		b.log.Info("bluetooth adapter", "state", state)
		if state != ble.StatePoweredOn {
			b.log.Warn("bluetooth adapter is not powered on; scanning will start once it is",
				"state", state,
				"hint", "check System Settings > Privacy & Security > Bluetooth if this stays unauthorized")
		}
	}

	mq, err := mqttc.New(ctx, mqttc.Options{
		Config:      b.cfg.MQTT,
		Logger:      b.log.With("component", "mqtt"),
		WillTopic:   b.bridgeAvailabilityTopic(),
		OnReconnect: b.republishAll,
	})
	if err != nil {
		return fmt.Errorf("connecting to MQTT: %w", err)
	}
	b.mq.Store(mq)
	defer mq.Close()

	b.subscribeCommands(mq)
	b.publishRetained(b.bridgeAvailabilityTopic(), mqttc.Online)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); b.eventPump(ctx) }()
	go func() { defer wg.Done(); b.scanController(ctx, scanUUIDs) }()
	go func() { defer wg.Done(); mq.Run(ctx) }()

	for _, m := range b.desks {
		wg.Add(1)
		go func(m *deskManager) { defer wg.Done(); m.run(ctx) }(m)
	}

	<-ctx.Done()
	b.log.Info("shutting down")
	wg.Wait()

	// Say goodbye explicitly; the retained will message only fires on an
	// unclean disconnect.
	for _, m := range b.desks {
		b.publishRetained(metricTopic(b.cfg.MQTT.MetricTopicBase, m.cfg.ID, leafAvailability), mqttc.Offline)
	}
	b.publishRetained(b.bridgeAvailabilityTopic(), mqttc.Offline)
	time.Sleep(200 * time.Millisecond) // let the final publishes drain before Disconnect
	return nil
}

func (b *Bridge) bridgeAvailabilityTopic() string {
	return metricTopic(b.cfg.MQTT.MetricTopicBase, config.ReservedDeskID, leafAvailability)
}

func (b *Bridge) subscribeCommands(mq *mqttc.Client) {
	base := b.cfg.MQTT.CommandTopicBase
	for _, m := range b.desks {
		m := m
		mq.Subscribe(commandTopic(base, m.cfg.ID, leafHeight), func(_ string, payload []byte) {
			m.submit(commandKindHeight, payload)
		})
		mq.Subscribe(commandTopic(base, m.cfg.ID, leafBaseHeight), func(_ string, payload []byte) {
			m.submit(commandKindBaseHeight, payload)
		})
	}
}

// publish sends a metric with the configured retain setting.
func (b *Bridge) publish(topic, payload string) {
	if mq := b.mq.Load(); mq != nil {
		mq.Publish(topic, payload)
	}
}

// publishRetained is for availability, which is state rather than an
// event and is always retained.
func (b *Bridge) publishRetained(topic, payload string) {
	if mq := b.mq.Load(); mq != nil {
		mq.PublishRetained(topic, payload)
	}
}

// republishAll re-announces every desk's state after an MQTT reconnect,
// because a broker restart loses the retained messages we published.
func (b *Bridge) republishAll() {
	b.publishRetained(b.bridgeAvailabilityTopic(), mqttc.Online)
	for _, m := range b.desks {
		m.republishState()
	}
}

// ---------------------------------------------------------------------
// Event pump
// ---------------------------------------------------------------------

// eventPump is the only reader of the BLE client's four event channels.
// corebluetooth-go drops events rather than blocking when a channel fills,
// so this loop stays non-blocking: every fan-out below is a buffered or
// best-effort send.
func (b *Bridge) eventPump(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return

		case p, ok := <-b.bleClient.Discoveries():
			if !ok {
				return
			}
			b.onDiscovery(p)

		case d, ok := <-b.bleClient.Disconnections():
			if !ok {
				return
			}
			b.onDisconnection(d)

		case s, ok := <-b.bleClient.StateChanges():
			if !ok {
				return
			}
			b.log.Info("bluetooth adapter state changed", "state", s)
			if s == ble.StatePoweredOn {
				b.wakeScanner()
			}

		case err, ok := <-b.bleClient.Errors():
			if !ok {
				return
			}
			b.log.Warn("bluetooth error", "err", err)
		}
	}
}

func (b *Bridge) onDiscovery(p ble.DiscoveredPeripheral) {
	// Log each peripheral once so `-log-level debug` answers "what is
	// nearby, and what is it called?" without drowning in advertisements.
	if _, dup := b.seen.LoadOrStore(strings.ToUpper(p.PeripheralID), struct{}{}); !dup {
		b.log.Debug("peripheral discovered",
			"peripheral_id", p.PeripheralID,
			"name", peripheralName(p),
			"rssi", p.RSSI,
			"advertised_services", p.AdvertisementData.ServiceUUIDs)
	}

	for _, m := range b.desks {
		if !m.wantResolve.Load() {
			continue
		}
		if !matchesDesk(m.cfg, p) {
			continue
		}
		if !m.wantResolve.CompareAndSwap(true, false) {
			continue
		}
		b.log.Info("desk found",
			"desk", m.cfg.ID, "peripheral_id", p.PeripheralID, "name", peripheralName(p), "rssi", p.RSSI)
		b.byPeripheral.Store(strings.ToUpper(p.PeripheralID), m)
		select {
		case m.resolved <- p.PeripheralID:
		default:
		}
		b.wakeScanner()
		return
	}
}

func (b *Bridge) onDisconnection(d ble.Disconnection) {
	v, ok := b.byPeripheral.Load(strings.ToUpper(d.PeripheralID))
	if !ok {
		return
	}
	m := v.(*deskManager)
	b.log.Info("desk disconnected", "desk", m.cfg.ID, "err", d.Err)
	select {
	case m.disconnected <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------
// Scanning
// ---------------------------------------------------------------------

func (b *Bridge) wakeScanner() {
	select {
	case b.scanWake <- struct{}{}:
	default:
	}
}

// scanController keeps a scan running exactly while at least one desk
// still needs its peripheral ID resolved. CoreBluetooth requires a
// peripheral to have been discovered in this process before it can be
// connected, so even a desk pinned by peripheral_id has to be seen in a
// scan first.
func (b *Bridge) scanController(ctx context.Context, serviceUUIDs []string) {
	scanning := false
	// The ticker is a safety net: it retries a scan that could not start
	// (adapter still powering on, permission not yet granted) without
	// anything having to wake us.
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	stop := func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := b.bleClient.StopScan(sctx); err != nil {
			b.log.Debug("stopping scan", "err", err)
		}
		scanning = false
		b.log.Debug("scan stopped")
	}

	for {
		select {
		case <-ctx.Done():
			if scanning {
				stop()
			}
			return
		case <-b.scanWake:
		case <-ticker.C:
		}

		need := false
		for _, m := range b.desks {
			if m.wantResolve.Load() {
				need = true
				break
			}
		}

		switch {
		case need && !scanning:
			sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := b.bleClient.StartScan(sctx, ble.ScanOptions{ServiceUUIDs: serviceUUIDs})
			cancel()
			if err != nil {
				b.log.Warn("could not start scanning, will retry", "err", err)
				continue
			}
			scanning = true
			if len(serviceUUIDs) == 0 {
				b.log.Info("scanning for desks (all peripherals; run with -scan to list what is nearby)")
			} else {
				b.log.Info("scanning for desks", "service_uuids", serviceUUIDs)
			}
		case !need && scanning:
			stop()
		}
	}
}

// matchesDesk reports whether a discovered peripheral is the configured
// desk. Config validation guarantees exactly one of the two fields is set.
func matchesDesk(d config.Desk, p ble.DiscoveredPeripheral) bool {
	if id := strings.TrimSpace(d.PeripheralID); id != "" {
		return strings.EqualFold(id, p.PeripheralID)
	}
	for _, got := range advertisedNames(p) {
		if want := strings.TrimSpace(d.Name); want != "" && strings.EqualFold(got, want) {
			return true
		}
		if want := strings.TrimSpace(d.NamePrefix); want != "" &&
			len(got) >= len(want) && strings.EqualFold(got[:len(want)], want) {
			return true
		}
	}
	return false
}

// advertisedNames returns the names a peripheral can be recognised by:
// the (possibly cached) peripheral name and the advertised local name.
// A DPG controller often supplies only one of the two.
func advertisedNames(p ble.DiscoveredPeripheral) []string {
	var names []string
	if p.Name != nil {
		if n := strings.TrimSpace(*p.Name); n != "" {
			names = append(names, n)
		}
	}
	if ln := p.AdvertisementData.LocalName; ln != nil {
		if n := strings.TrimSpace(*ln); n != "" && (len(names) == 0 || !strings.EqualFold(names[0], n)) {
			names = append(names, n)
		}
	}
	return names
}

func peripheralName(p ble.DiscoveredPeripheral) string {
	if p.Name != nil && *p.Name != "" {
		return *p.Name
	}
	if ln := p.AdvertisementData.LocalName; ln != nil {
		return *ln
	}
	return ""
}

// ---------------------------------------------------------------------
// Helper stderr plumbing
// ---------------------------------------------------------------------

// lineWriter turns the helper's stderr into log lines instead of letting
// it interleave with our own output on the terminal.
type lineWriter struct {
	log    *slog.Logger
	source string
	w      *io.PipeWriter
}

func newLineWriter(log *slog.Logger, source string) io.Writer {
	pr, pw := io.Pipe()
	lw := &lineWriter{log: log, source: source, w: pw}
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" {
				log.Debug(line, "source", source)
			}
		}
	}()
	return lw
}

func (w *lineWriter) Write(p []byte) (int, error) { return w.w.Write(p) }
