# mqtt-linak

An MQTT bridge for LINAK DPG sit/stand desk controllers, on macOS.

It connects to one or more desks over Bluetooth LE, keeps those
connections up for as long as it runs, publishes each desk's height as it
changes, and moves desks in response to commands published to MQTT.

```
MQTT broker  <--->  mqtt-linak  <--->  corebluetoothd  <--->  LINAK DPG desk
                    (this repo)       (corebluetooth-go)      (dpg protocol)
```

- [`corebluetooth-go`](https://github.com/gomi-source/corebluetooth-go) supplies the BLE transport: a
  Swift helper process owning the `CBCentralManager`, spoken to over a
  Unix socket, so none of this needs cgo.
- [`linak-dpg`](https://github.com/gomi-source/linak-dpg) supplies the LINAK protocol: service/characteristic
  UUIDs, the DeskPanel command envelope, and the `desk` package's `Move`,
  `BaseOffset` and `WriteBaseOffset`.
- This repo supplies the supervision (scan, connect, reconnect) and the
  MQTT mapping.

Both sibling repos are resolved through `replace` directives in `go.mod`,
so all three need to sit next to each other in the same parent directory.

## Topics

`{deskId}` is the `id` you give a desk in the config file, not its
Bluetooth name or UUID — so topics stay stable if the desk is renamed or
the bridge moves to a different Mac.

All heights are integers in **tenths of a millimetre**, the unit the desk
itself uses, sent and received as a plain decimal string (`7350`).

### Commands — subscribed

| Topic | Effect |
|---|---|
| `{command_topic_base}/{deskId}/height` | Move the desk to this height above its base. |
| `{command_topic_base}/{deskId}/base_height` | Store the base's distance from the floor on the controller. |

### Metrics — published

| Topic | Meaning |
|---|---|
| `{metric_topic_base}/{deskId}/height` | Current height above the base, as the desk reports it. |
| `{metric_topic_base}/{deskId}/base_height` | The controller's stored base height, read on every connect. |
| `{metric_topic_base}/{deskId}/availability` | `online` / `offline` for that desk's BLE connection. |
| `{metric_topic_base}/bridge/availability` | `online` / `offline` for the bridge itself (also the MQTT last will). |

`bridge` is therefore a reserved desk id, and the config file rejects it.

### Two reference frames

`height` is measured **from the desk's own base**, not from the floor —
that is what the ReferenceOutput characteristic reports and what `Move`
accepts, so commands and metrics share one frame and no arithmetic sits
between them. `base_height` is the separate, rarely-changing offset from
the floor to that base, which the desk stores for itself. Height above
the floor is `base_height + height`; the bridge does not compute it.

Writing `base_height` re-reads the value afterwards, so the metric
reflects what the controller actually stored rather than what was asked
for.

### Payloads

Commands are normally just the number:

```sh
mosquitto_pub -t cmd/linak/anders/height -m 7350
```

A JSON object with a `value`, `height`, `position` or `base_height` field
is also accepted, so a home-automation system that can only emit JSON
templates needs no extra templating. Fractional values are rounded;
anything outside `0..65535` is rejected and logged.

Metrics are always a plain decimal string, and retained by default so a
subscriber that connects later immediately sees each desk's last known
position.

## Configuration

Copy `config.example.yaml` to `config.yaml`. Only `mqtt.broker` and the
desks list are required:

```yaml
mqtt:
  broker: mqtt://localhost:1883

desks:
  - id: anders
    name: "Desk 8802"
```

Each desk sets exactly one of:

| Field | Matched against |
|---|---|
| `name` | The peripheral name and the advertised local name, exactly, case-insensitively. |
| `name_prefix` | The same, as a prefix — for `Desk 8802` style names where the suffix varies. |
| `peripheral_id` | The CoreBluetooth peripheral UUID. Stable on one Mac, different on another Mac for the same physical desk. |

### Finding the desk

```sh
./bin/mqtt-linak -scan
```

scans for twelve seconds (`-scan-duration` to change that) and prints
every peripheral it saw with its UUID, name, signal strength and
advertised services, followed by a ready-to-paste `desks:` entry. No
config file is needed for this. If the desk's panel is asleep, press a
button on it first — some controllers stop advertising when idle.

**There is no service UUID to filter the scan on.** A LINAK DPG
controller does not put its control service
(`99FA0001-338A-1024-8A49-009C0215F78A`) in its advertisement; that
service only becomes visible after connecting and discovering, and
CoreBluetooth matches scan filters against the advertisement alone. So
the bridge scans unfiltered by default and identifies the desk by name or
UUID instead. `bluetooth.scan_service_uuids` can still narrow the scan to
whatever your controller *does* advertise — `-scan` shows you what that
is.

These environment variables override the file, which is the intended way
to keep broker credentials out of it:

`MQTT_LINAK_CONFIG` (config path), `MQTT_LINAK_BROKER`,
`MQTT_LINAK_CLIENT_ID`, `MQTT_LINAK_USERNAME`, `MQTT_LINAK_PASSWORD`,
`MQTT_LINAK_COMMAND_TOPIC_BASE`, `MQTT_LINAK_METRIC_TOPIC_BASE`,
`MQTT_LINAK_HELPER_PATH`, `MQTT_LINAK_QOS`, `MQTT_LINAK_RETAIN`.

## Building and running

Requires macOS with a Swift toolchain (for the helper) and Go 1.25.

```sh
go mod tidy          # first time only; writes go.sum
make                 # builds bin/mqtt-linak and bin/corebluetoothd.app
cp config.example.yaml config.yaml && $EDITOR config.yaml
./bin/mqtt-linak -config config.yaml
```

`make helper` builds `corebluetoothd` from the sibling checkout and
copies the `.app` bundle into `bin/`, which is where `ble.Start()` looks
for it. The bundle, rather than a bare binary, is required: macOS reads
the CoreBluetooth authorization identity from its `Info.plist`, and a
bare binary is killed outright the moment it touches `CBCentralManager`.

The first scan triggers the Bluetooth permission prompt. If it never
appears, or scanning silently finds nothing, check **System Settings >
Privacy & Security > Bluetooth**.

Flags: `-config`, `-scan`, `-scan-duration`, `-log-level`
(`debug`/`info`/`warn`/`error`), `-log-format` (`text`/`json`),
`-mqtt-debug`, `-version`.

`-mqtt-debug` (or `mqtt.debug` in the config) adds paho's own protocol
trace, which logs every keepalive ping and inbound packet — a handful of
lines every few seconds, indefinitely. It is deliberately not tied to
`-log-level debug`, because it buries the bridge's own debug output.
paho's errors and warnings are logged either way, so a broker that
refuses the connection still says why without it.

`-log-level debug` also logs every peripheral seen during normal
operation, once each, with its advertised services — useful when a desk
that used to be found stops matching — and every command sent to a desk
as `desk command`, in order, with its parameters:

```
level=DEBUG msg="desk command" desk=anders command=wake_up
level=DEBUG msg="desk command" desk=anders command=stop
level=DEBUG msg="desk command" desk=anders command=take_ownership
level=DEBUG msg="desk command" desk=anders command=read_base_height
level=DEBUG msg="desk command" desk=anders command=move target_tenths_mm=7350
level=DEBUG msg="desk command" desk=anders command=keepalive_wake_up
```

A failing one is logged again at warn with its error, so a command that
the desk accepted and then ignored is distinguishable from one that never
went out.

The bytes on the wire are logged too: the per-desk logger is handed to
`dpg` (`dpg.NewDevice(...).WithLogger(log)`), so every GATT write and
every notification appears at debug level alongside the command that
provoked it, tagged with the same `desk=` attribute:

```
level=DEBUG msg="desk command" desk=anders command=move target_tenths_mm=7350
level=DEBUG msg="gatt write" desk=anders characteristic=99FA0031 bytes="B6 1C" with_response=false
level=DEBUG msg="gatt notification" desk=anders characteristic=99FA0021 bytes="9A 1B 00 00"
```

## How the connection is kept up

Each desk gets a goroutine running scan → connect → serve → back off →
repeat, for the process's lifetime.

- **Scanning is shared, unfiltered and refcounted.** CoreBluetooth will
  not connect to a peripheral this process has never discovered, so even a
  desk pinned by `peripheral_id` has to be seen in a scan first. One scan
  runs while any desk still needs resolving, and stops when none do. It
  cannot be filtered by the control service — see "Finding the desk"
  above.
- **Reconnects are backed off** from `reconnect_min_backoff` to
  `reconnect_max_backoff`, with jitter so several desks that dropped
  together don't retry in lockstep. Three consecutive failures discard the
  peripheral ID and go back to scanning, in case the helper restarted or
  the desk was re-paired.
- **The backoff only resets after a connection that lasted.** A drop after
  a minute or more of useful service is treated as a one-off and retried
  promptly. A drop sooner than that leaves the backoff growing — resetting
  it on every clean disconnect is what turns a flapping desk into a
  reconnect loop retrying at the minimum interval forever. The log records
  how long each connection was held, which is the quickest way to tell a
  flapping link from an ordinary one.
- **An idle desk gets a wake-up before it times out.** A DPG controller
  drops a connection that has been idle for four hours — consistently
  enough to be a deliberate timeout rather than a fault. So when nothing
  has been sent to a desk for `keepalive_interval` (default one hour), a
  Control wake-up goes out. Any command counts as activity, so a desk in
  use never sees one, and the default leaves several attempts before the
  four hours are up.

  **Only outbound traffic resets the clock**, which is observed rather
  than assumed: commanding a move over MQTT resets the timer, while moving
  the desk from its own panel does not — and that produces a steady stream
  of inbound position reports and no writes at all. The controller counts
  what it is told, not what it says. So the keepalive tracks writes only,
  and a desk someone is using by hand still gets one.

  That also settles that the four hours are about idleness rather than the
  age of the connection. What is left to confirm is narrower: a move is a
  ReferenceInput write and the keepalive is a Control write, so the
  running bridge is what shows whether that characteristic counts too. A
  drop at four hours with hourly wake-ups in the log would mean it does
  not, and the keepalive should then send something else — a base-offset
  read would do.
- **A wake-up and a stop are sent on every connect**, before anything
  else, which is what LINAK's own app does on startup. A DPG controller
  can get into a state where it drops the link repeatedly; a wake-up
  appears to end that, and the effect seems to persist across
  connections rather than being a per-session handshake. Ordinary
  occasional drops continue regardless. Turn it off with
  `bluetooth.wake_on_connect: false`.
- **Ownership is re-asserted on every connect.** A DPG controller
  silently ignores every write — moves included — unless the user ID it
  has stored carries the owner bit, and it clears that bit by itself, so
  checking once at startup is not enough. Each connect calls
  `desk.TakeOwnership`, which reads the stored user ID and, if the bit is
  clear, sets it and writes it back. Failure is logged but not fatal:
  reading keeps working and the next reconnect tries again.
- **GATT state is rebuilt on every connect.** CoreBluetooth invalidates
  service and characteristic objects across a disconnect and `dpg` caches
  its own handles per peripheral, so both caches are dropped and
  rediscovered each time.
- **Notification callbacks are registered once.** `dpg`'s notify registry
  has no unsubscribe path, so re-running `EnableNotifications` after a
  reconnect would leave the old callback in place and publish every
  reading twice. Instead the callbacks are registered on the first
  connect, and later connects only re-arm the characteristic through the
  BLE client directly.
- **MQTT reconnects are handled too**, by our own loop rather than paho's.
  Subscriptions are replayed and all retained state is republished after
  each reconnect, since a broker restart loses retained messages. Every
  failed attempt is logged with its reason — paho's own AutoReconnect and
  ConnectRetry report that only to their DEBUG logger, which makes a
  client that never connects look like one that has nothing to say.
- **Height updates are coalesced.** A moving desk pushes a notification
  every few tens of milliseconds; those are rate-limited to
  `publish_min_interval`, unchanged values are dropped, and the final
  resting position (the desk reporting speed 0) is always published
  immediately.

## Scope and known limitations

MVP: height and base height only. The `dpg` module also exposes
capabilities, product info, memory positions, reminder settings, user ID
and control errors, none of which are surfaced here yet — nor is the
desk's movement speed, which the bridge reads but only uses to decide when
a move has finished.

- **macOS only.** CoreBluetooth is the only backend; the Go code compiles
  anywhere but `ble.Start()` refuses to run off darwin.
- **One process, one Mac.** `corebluetooth-go` spawns a private helper per
  client and CoreBluetooth allows one `CBCentralManager` per process.
- **Commands while a desk is offline are dropped**, with a warning, rather
  than queued. The command queue is 8 deep; a flood is dropped rather than
  blocking the MQTT callback goroutine.
- **Upstream: a `dpg` subscription's dispatcher cannot be removed** from
  the notify registry once started, which is why the position stream here
  is created once and never rebuilt; reconnects re-arm the characteristic
  through the BLE client instead. Only ReferenceOutput is subscribed to at
  all now — base height is read synchronously with `desk.BaseOffset(ctx)`,
  since DeskPanel is request/response.
- **Everything that writes needs the owner bit** on the controller, which
  the bridge sets on each connect (see above). If `TakeOwnership` keeps
  failing, moves will be accepted over MQTT and then silently do nothing.

## Development

```sh
make test     # go test ./...
make vet
make fmt
go test -race ./...
```

Tests cover config parsing, defaulting, env overrides and validation;
command payload parsing; topic construction; desk matching against
advertisements; and the publish throttle (against an injected clock, so
they don't sleep). The BLE and MQTT layers are not unit tested — they need
a real desk and a real broker.
