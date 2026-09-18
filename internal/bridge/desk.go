package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gomi-source/linak-dpg"
	deskpkg "github.com/gomi-source/linak-dpg/desk"
	"github.com/gomi-source/linak-dpg/subscription/referenceoutput"

	"github.com/gomi-source/mqtt-linak/internal/config"
	"github.com/gomi-source/mqtt-linak/internal/mqttc"
)

type commandKind int

const (
	commandKindHeight commandKind = iota
	commandKindBaseHeight
)

func (k commandKind) String() string {
	if k == commandKindBaseHeight {
		return leafBaseHeight
	}
	return leafHeight
}

type command struct {
	kind  commandKind
	value int
}

// errDisconnected ends a serve loop because the desk went away, as opposed
// to the bridge shutting down.
var errDisconnected = errors.New("desk disconnected")

// stableConnection is how long a connection has to last before a drop is
// treated as a one-off rather than as flapping. A desk that drops shortly
// after every connect would otherwise reset the backoff every time and be
// retried at the minimum interval indefinitely.
const stableConnection = 60 * time.Second

// deskManager owns one physical desk for the lifetime of the process: it
// resolves it in a scan, keeps it connected, mirrors its notifications to
// MQTT, and applies commands arriving from MQTT.
type deskManager struct {
	b   *Bridge
	cfg config.Desk
	log *slog.Logger

	// wantResolve is set while this desk still needs a peripheral ID; the
	// bridge's scan controller keeps a scan running while any desk wants
	// one, and the event pump hands the ID over on resolved.
	wantResolve atomic.Bool
	resolved    chan string

	peripheralID string

	// disconnected is poked by the event pump when this desk's peripheral
	// drops, so the serve loop stops waiting.
	disconnected chan struct{}

	commands chan command

	// desk and its position stream are created once, on the first
	// successful connect, and then reused across reconnects. A
	// subscription's dispatcher cannot be removed from dpg's notify
	// registry once started, so rebuilding it per reconnect would leave
	// the old one registered and publish every reading twice.
	desk    *deskpkg.Desk
	roSub   referenceoutput.Subscribable
	subsSet bool

	gatt *dpg.GATT

	height *throttle

	// Last values actually published, kept so state can be re-announced
	// after an MQTT reconnect wipes the broker's retained messages.
	stateMu       sync.Mutex
	lastHeight    int
	hasHeight     bool
	lastBase      int
	hasBase       bool
	online        bool
	onlineAt      time.Time
	lastHeld      time.Duration
	consecFailure int
}

func newDeskManager(b *Bridge, cfg config.Desk) *deskManager {
	m := &deskManager{
		b:            b,
		cfg:          cfg,
		log:          b.log.With("desk", cfg.ID),
		resolved:     make(chan string, 1),
		disconnected: make(chan struct{}, 1),
		commands:     make(chan command, 8),
		gatt:         &dpg.GATT{},
	}
	m.height = newThrottle(b.cfg.Bluetooth.PublishMinInterval.D(), m.publishHeight)
	return m
}

// run is the desk's whole lifecycle: resolve, connect, serve, repeat.
func (m *deskManager) run(ctx context.Context) {
	minBackoff := m.b.cfg.Bluetooth.ReconnectMinBackoff.D()
	maxBackoff := m.b.cfg.Bluetooth.ReconnectMaxBackoff.D()
	backoff := minBackoff

	for ctx.Err() == nil {
		if m.peripheralID == "" {
			if !m.resolve(ctx) {
				return
			}
		}

		err := m.connectAndServe(ctx)
		m.setOnline(false)
		m.b.publishRetained(m.topic(leafAvailability), mqttc.Offline)

		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, errDisconnected):
			held := m.heldFor()
			if held >= stableConnection {
				// The connection did its job before dropping, so this is a
				// one-off: retry promptly.
				m.log.Info("reconnecting", "held", held.Round(time.Second), "in", backoff.Round(time.Millisecond))
				backoff = minBackoff
			} else {
				// Dropped almost immediately. Resetting the backoff here is
				// what turns a flapping desk into a reconnect loop, so let
				// it keep growing instead.
				m.log.Warn("desk dropped soon after connecting",
					"held", held.Round(time.Millisecond), "retry_in", backoff.Round(time.Millisecond))
			}
		case err != nil:
			m.consecFailure++
			m.log.Warn("connection attempt failed", "err", err, "retry_in", backoff.Round(time.Millisecond), "consecutive_failures", m.consecFailure)
			// After a few failures the peripheral ID is the likely
			// suspect (helper restarted, desk re-paired, config points at
			// a stale UUID), so go back to scanning for it.
			if m.consecFailure >= 3 {
				m.log.Info("re-scanning for this desk")
				m.forgetPeripheral()
			}
		}

		if !sleepCtx(ctx, jitter(backoff)) {
			return
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// resolve asks the scan controller to look for this desk and waits for the
// event pump to hand back a peripheral ID. It returns false if ctx ended.
func (m *deskManager) resolve(ctx context.Context) bool {
	m.wantResolve.Store(true)
	m.b.wakeScanner()
	m.log.Info("looking for desk", "match", m.matchDescription())

	select {
	case <-ctx.Done():
		return false
	case pid := <-m.resolved:
		m.peripheralID = pid
		return true
	}
}

func (m *deskManager) forgetPeripheral() {
	if m.peripheralID != "" {
		m.b.byPeripheral.Delete(upper(m.peripheralID))
	}
	m.peripheralID = ""
	m.consecFailure = 0
}

func (m *deskManager) matchDescription() string {
	if m.cfg.PeripheralID != "" {
		return "peripheral_id=" + m.cfg.PeripheralID
	}
	return "name=" + m.cfg.Name
}

// connectAndServe connects, (re)establishes GATT state, then blocks
// handling commands until the desk disconnects or ctx ends.
func (m *deskManager) connectAndServe(ctx context.Context) error {
	connectTimeout := m.b.cfg.Bluetooth.ConnectTimeout.D()

	// Drain a disconnect left over from the previous connection so it
	// doesn't immediately end this one.
	select {
	case <-m.disconnected:
	default:
	}

	m.log.Info("connecting", "peripheral_id", m.peripheralID)
	cctx, cancel := context.WithTimeout(ctx, connectTimeout+5*time.Second)
	err := m.b.bleClient.Connect(cctx, m.peripheralID, connectTimeout)
	cancel()
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	device := dpg.NewDevice(m.b.bleClient, m.peripheralID).WithLogger(m.log)
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = m.b.bleClient.Disconnect(dctx, m.peripheralID)
		dcancel()
	}()

	// CoreBluetooth invalidates a peripheral's CBService/CBCharacteristic
	// objects across a disconnect, and dpg caches its own handles per
	// peripheral ID, so both sides have to be rediscovered every time.
	m.gatt.Disconnect(device)
	if err := m.discover(device); err != nil {
		return fmt.Errorf("discover: %w", err)
	}
	if err := m.ensureSubscriptions(ctx, device); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	// LINAK's own app opens a session with a wake-up and a stop, and a
	// controller that has got into a state of dropping the link
	// repeatedly appears to come out of it once one is sent. Neither is
	// expensive, and both are sent before anything else so that a
	// misbehaving desk is listening by the time the ownership exchange
	// starts.
	if m.b.cfg.Bluetooth.WakeOnConnect {
		_ = m.command("wake_up", m.desk.WakeUp)
		_ = m.command("stop", m.desk.Stop)
	}

	// The desk ignores every write - moves included - unless the user ID
	// it has stored carries the owner bit, and it clears that bit by
	// itself, so it has to be re-checked on each connect rather than once
	// at setup. A failure here is not fatal: reading still works, and the
	// next reconnect tries again.
	var changed bool
	ownErr := m.command("take_ownership", func() error {
		var serr error
		changed, serr = m.desk.TakeOwnership(ctx)
		return serr
	})
	switch {
	case ownErr != nil:
		m.log.Warn("could not take ownership of the desk; it will ignore moves until this succeeds", "err", ownErr)
	case changed:
		m.log.Info("owner bit set on the desk")
	default:
		m.log.Debug("already the desk's owner")
	}

	m.consecFailure = 0
	m.setOnline(true)
	m.height.Reset() // re-announce the current height even if unchanged
	m.b.publishRetained(m.topic(leafAvailability), mqttc.Online)
	m.log.Info("desk connected")

	// The base offset is not pushed spontaneously; read it so the
	// base_height metric is populated on every connect.
	m.readBaseHeight(ctx)

	flush := time.NewTicker(maxDuration(m.b.cfg.Bluetooth.PublishMinInterval.D(), 50*time.Millisecond))
	defer flush.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-m.disconnected:
			return errDisconnected
		case <-flush.C:
			m.height.Flush()
		case cmd := <-m.commands:
			m.apply(ctx, cmd)
		}
	}
}

// command runs one operation against the desk, logging it before it goes
// out and again if it fails. Every BLE write the bridge makes goes
// through here, so -log-level debug shows exactly what was asked of the
// desk and in what order - which is the first thing worth knowing when a
// desk accepts a command and then does nothing.
func (m *deskManager) command(name string, fn func() error, attrs ...any) error {
	args := make([]any, 0, len(attrs)+2)
	args = append(args, "command", name)
	args = append(args, attrs...)

	m.log.Debug("desk command", args...)

	err := fn()
	if err != nil {
		failed := make([]any, 0, len(args)+2)
		failed = append(failed, args...)
		failed = append(failed, "err", err)
		m.log.Warn("desk command failed", failed...)
	}
	return err
}

// discover warms dpg's GATT cache for every characteristic this bridge
// uses. Doing it up front turns a mid-command discovery failure into a
// connection failure we can retry, and guarantees the helper has
// discovered the services before SetNotify is called on reconnect.
func (m *deskManager) discover(device dpg.Device) error {
	wanted := []struct {
		name    string
		service string
		char    string
	}{
		{"reference output", dpg.ServiceUUIDReferenceOutput, dpg.CharacteristicUUIDReferenceOutput},
		{"reference input", dpg.ServiceUUIDReferenceInput, dpg.CharacteristicUUIDReferenceInput},
		{"desk panel", dpg.ServiceUUIDDeskPanel, dpg.CharacteristicUUIDDeskPanel},
	}
	for _, w := range wanted {
		if _, err := m.gatt.GetCharacteristic(device, w.service, w.char); err != nil {
			return fmt.Errorf("%s characteristic: %w", w.name, err)
		}
	}
	return nil
}

// ensureSubscriptions registers the notification callbacks once and, on
// every later connect, only re-enables notifications on the peripheral.
//
// dpg's Characteristic.EnableNotifications both flips the CCCD and appends
// a callback to a process-wide registry that has no removal path, so
// calling it again after a reconnect would double every reading. Toggling
// notify through the BLE client directly re-arms the peripheral while
// leaving the single registered callback in place.
func (m *deskManager) ensureSubscriptions(ctx context.Context, device dpg.Device) error {
	if m.subsSet {
		for _, c := range []struct{ service, char string }{
			{dpg.ServiceUUIDReferenceOutput, dpg.CharacteristicUUIDReferenceOutput},
			{dpg.ServiceUUIDDeskPanel, dpg.CharacteristicUUIDDeskPanel},
		} {
			nctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := m.b.bleClient.SetNotify(nctx, m.peripheralID, c.service, c.char, true)
			cancel()
			if err != nil {
				return fmt.Errorf("re-enabling notifications on %s: %w", c.char, err)
			}
		}
		return nil
	}

	m.desk = deskpkg.New(device, m.cfg.ID)
	if m.desk == nil || m.desk.Device().IsZero() {
		return errors.New("desk could not be created from the connected device")
	}

	// ReferenceOutput is the only stream: the desk pushes its position as
	// it moves. Everything on DeskPanel is request/response and is read
	// through the desk's own methods instead, so there is nothing to
	// subscribe to there.
	m.roSub = m.desk.Positions()
	m.roSub.AddReferenceOutputCallback(m.onReferenceOutput)

	m.subsSet = true
	return nil
}

// ---------------------------------------------------------------------
// Notifications in -> MQTT out
// ---------------------------------------------------------------------

// onReferenceOutput receives the desk's position notifications. extension
// is the height above the desk's own base in tenths of a millimetre, which
// is the same reference frame Move() takes, so it is published verbatim as
// the height metric. speed is non-zero while the desk is moving; a zero
// reading means it has settled, which is always worth publishing at once.
func (m *deskManager) onReferenceOutput(extension, speed int) {
	m.height.Set(extension, speed == 0)
}

func (m *deskManager) publishHeight(v int) {
	m.stateMu.Lock()
	m.lastHeight, m.hasHeight = v, true
	m.stateMu.Unlock()
	m.b.publish(m.topic(leafHeight), formatTenthsMM(v))
}

// readBaseHeight asks the desk for its configured distance from the floor
// to its lowest position and republishes it. The value changes only when
// written, so it is read on connect and after a write rather than watched.
func (m *deskManager) readBaseHeight(ctx context.Context) {
	var v int
	if err := m.command("read_base_height", func() error {
		var rerr error
		v, rerr = m.desk.BaseOffset(ctx)
		return rerr
	}); err != nil {
		return
	}
	m.onBaseOffset(v)
}

// onBaseOffset records and publishes a base offset just read.
func (m *deskManager) onBaseOffset(v int) {
	m.stateMu.Lock()
	changed := !m.hasBase || m.lastBase != v
	m.lastBase, m.hasBase = v, true
	m.stateMu.Unlock()
	if changed {
		m.log.Debug("base height", "tenths_mm", v)
	}
	m.b.publish(m.topic(leafBaseHeight), formatTenthsMM(v))
}

// republishState re-announces everything this desk has published, after an
// MQTT reconnect has lost the broker's retained messages.
func (m *deskManager) republishState() {
	m.stateMu.Lock()
	online, h, hasH, base, hasBase := m.online, m.lastHeight, m.hasHeight, m.lastBase, m.hasBase
	m.stateMu.Unlock()

	avail := mqttc.Offline
	if online {
		avail = mqttc.Online
	}
	m.b.publishRetained(m.topic(leafAvailability), avail)
	if hasH {
		m.b.publish(m.topic(leafHeight), formatTenthsMM(h))
	}
	if hasBase {
		m.b.publish(m.topic(leafBaseHeight), formatTenthsMM(base))
	}
}

func (m *deskManager) setOnline(v bool) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()

	m.online = v
	if v {
		m.onlineAt = time.Now()
		return
	}
	if !m.onlineAt.IsZero() {
		m.lastHeld = time.Since(m.onlineAt)
		m.onlineAt = time.Time{}
	} else {
		m.lastHeld = 0
	}
}

// heldFor reports how long the last connection stayed up, or zero if it
// never got as far as being usable.
func (m *deskManager) heldFor() time.Duration {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.lastHeld
}

func (m *deskManager) isOnline() bool {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.online
}

func (m *deskManager) topic(leaf string) string {
	return metricTopic(m.b.cfg.MQTT.MetricTopicBase, m.cfg.ID, leaf)
}

// ---------------------------------------------------------------------
// MQTT in -> desk out
// ---------------------------------------------------------------------

// submit parses a command payload and queues it. It runs on a paho
// callback goroutine, so it never blocks: a full queue means the desk is
// not keeping up and the stale command is dropped.
func (m *deskManager) submit(kind commandKind, payload []byte) {
	value, err := parseTenthsMM(payload)
	if err != nil {
		m.log.Warn("ignoring command", "command", kind, "err", err, "payload", truncate(string(payload), 64))
		return
	}
	if !m.isOnline() {
		m.log.Warn("ignoring command, desk is not connected", "command", kind, "value", value)
		return
	}
	select {
	case m.commands <- command{kind: kind, value: value}:
	default:
		m.log.Warn("command queue full, dropping command", "command", kind, "value", value)
	}
}

func (m *deskManager) apply(ctx context.Context, cmd command) {
	switch cmd.kind {
	case commandKindHeight:
		m.log.Info("moving desk", "target_tenths_mm", cmd.value)
		_ = m.command("move", func() error { return m.desk.Move(cmd.value) },
			"target_tenths_mm", cmd.value)

	case commandKindBaseHeight:
		m.log.Info("setting base height", "tenths_mm", cmd.value)
		if err := m.command("write_base_height",
			func() error { return m.desk.WriteBaseOffset(cmd.value) },
			"tenths_mm", cmd.value); err != nil {
			return
		}
		// Read it back so the metric reflects what the desk actually
		// stored (the write is rejected unless this user owns the desk).
		m.readBaseHeight(ctx)
	}
}

// ---------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// jitter spreads reconnect attempts so several desks that dropped together
// (a Bluetooth reset, say) do not retry in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d + time.Duration(rand.Int63n(int64(d/4)+1))
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func upper(s string) string {
	return strings.ToUpper(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
