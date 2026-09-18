// Package config loads and validates the bridge's YAML configuration.
//
// Everything has a usable default except the MQTT broker URL and the desk
// list, so a minimal config file is a handful of lines. A few settings can
// additionally be overridden from the environment (see ApplyEnv), which is
// the intended way to keep broker credentials out of the file.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from a YAML string such as
// "5s" or "1m30s". yaml.v3 would otherwise insist on a raw nanosecond
// count, which is unreadable in a hand-edited config file.
type Duration time.Duration

// D returns the value as a plain time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"5s\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// Config is the whole configuration file.
type Config struct {
	MQTT      MQTT      `yaml:"mqtt"`
	Bluetooth Bluetooth `yaml:"bluetooth"`
	Desks     []Desk    `yaml:"desks"`
}

// MQTT configures the broker connection and the topic namespaces.
type MQTT struct {
	// Broker is a paho-style broker URL, e.g. tcp://192.168.1.10:1883,
	// ssl://broker:8883 or ws://broker:9001/mqtt.
	Broker   string `yaml:"broker"`
	ClientID string `yaml:"client_id"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`

	// QoS applies to both published metrics and command subscriptions.
	QoS byte `yaml:"qos"`
	// Retain marks published metrics as retained, so a subscriber that
	// connects later immediately sees each desk's last known height.
	Retain bool `yaml:"retain"`

	// CommandTopicBase is the prefix the bridge subscribes under:
	//   {command_topic_base}/{deskId}/height
	//   {command_topic_base}/{deskId}/base_height
	CommandTopicBase string `yaml:"command_topic_base"`
	// MetricTopicBase is the prefix the bridge publishes under:
	//   {metric_topic_base}/{deskId}/height
	//   {metric_topic_base}/{deskId}/base_height
	//   {metric_topic_base}/{deskId}/availability
	//   {metric_topic_base}/bridge/availability
	MetricTopicBase string `yaml:"metric_topic_base"`

	// Debug turns on paho's own DEBUG stream, which logs every keepalive
	// ping and inbound packet - several lines every few seconds, forever.
	// It is separate from -log-level because it drowns everything else;
	// paho's errors and warnings are logged regardless of this setting.
	Debug bool `yaml:"debug"`

	KeepAlive      Duration `yaml:"keep_alive"`
	ConnectTimeout Duration `yaml:"connect_timeout"`
}

// Bluetooth configures scanning, connecting and the metric publish rate.
type Bluetooth struct {
	// HelperPath optionally points at the corebluetoothd binary. When
	// empty, corebluetooth-go looks next to the running executable and
	// then on $PATH.
	HelperPath string `yaml:"helper_path"`

	// ScanServiceUUIDs restricts scanning to peripherals advertising one
	// of these service UUIDs. Unset means scan for everything, which is
	// the default and usually the only thing that works: a LINAK
	// controller does not advertise its control service, so filtering on
	// 99FA0001-... finds nothing. Use `mqtt-linak -scan` to see what a
	// particular controller does advertise before narrowing this.
	ScanServiceUUIDs []string `yaml:"scan_service_uuids"`

	// WakeOnConnect sends a Control wake-up, then a stop, on every
	// connect - which is what LINAK's own iPhone app does on startup. A
	// DPG controller can get into a state where it drops the link
	// repeatedly, and a wake-up appears to bring it out of that; the
	// effect seems to persist across connections rather than being a
	// per-session handshake. Defaults to on.
	WakeOnConnect bool `yaml:"wake_on_connect"`

	ConnectTimeout      Duration `yaml:"connect_timeout"`
	ReconnectMinBackoff Duration `yaml:"reconnect_min_backoff"`
	ReconnectMaxBackoff Duration `yaml:"reconnect_max_backoff"`

	// PublishMinInterval coalesces height updates while the desk is
	// moving; the final position (speed 0) is always published
	// immediately regardless.
	PublishMinInterval Duration `yaml:"publish_min_interval"`
}

// Desk maps one physical desk to one topic namespace.
//
// Exactly one of Name, NamePrefix or PeripheralID selects the desk during
// scanning. Name and NamePrefix are matched case-insensitively against
// both the peripheral name and the advertised local name; PeripheralID is
// matched against the CoreBluetooth peripheral UUID, which is stable on
// one Mac but differs between Macs for the same physical desk.
type Desk struct {
	ID           string `yaml:"id"`
	Name         string `yaml:"name"`
	NamePrefix   string `yaml:"name_prefix"`
	PeripheralID string `yaml:"peripheral_id"`
}

// ReservedDeskID is used for the bridge's own availability topic and so
// cannot also name a desk.
const ReservedDeskID = "bridge"

// DefaultConfig returns the configuration used when the file omits a
// setting. It is not usable on its own: Broker and Desks have no
// meaningful default.
func DefaultConfig() Config {
	return Config{
		MQTT: MQTT{
			ClientID:         "mqtt-linak",
			QoS:              0,
			Retain:           true,
			CommandTopicBase: "cmd/linak",
			MetricTopicBase:  "tele/linak",
			KeepAlive:        Duration(30 * time.Second),
			ConnectTimeout:   Duration(10 * time.Second),
		},
		Bluetooth: Bluetooth{
			WakeOnConnect:       true,
			ScanServiceUUIDs:    nil, // nil means unfiltered; see bridge.DefaultScanServiceUUIDs
			ConnectTimeout:      Duration(30 * time.Second),
			ReconnectMinBackoff: Duration(2 * time.Second),
			ReconnectMaxBackoff: Duration(60 * time.Second),
			PublishMinInterval:  Duration(250 * time.Millisecond),
		},
	}
}

// Load reads, defaults, env-overrides and validates a config file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	return Parse(raw)
}

// Parse does everything Load does, given the file contents directly.
func Parse(raw []byte) (*Config, error) {
	cfg := DefaultConfig()

	// A sentinel so an explicit "scan_service_uuids: []" (scan for
	// everything) is distinguishable from the key being absent.
	var probe struct {
		Bluetooth struct {
			ScanServiceUUIDs *[]string `yaml:"scan_service_uuids"`
		} `yaml:"bluetooth"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if probe.Bluetooth.ScanServiceUUIDs == nil {
		cfg.Bluetooth.ScanServiceUUIDs = nil // caller substitutes the default
	} else {
		cfg.Bluetooth.ScanServiceUUIDs = *probe.Bluetooth.ScanServiceUUIDs
		if cfg.Bluetooth.ScanServiceUUIDs == nil {
			cfg.Bluetooth.ScanServiceUUIDs = []string{}
		}
	}

	cfg.ApplyEnv(os.LookupEnv)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// ApplyEnv overlays MQTT_LINAK_* environment variables onto the config.
// lookup is os.LookupEnv in production and a stub in tests.
func (c *Config) ApplyEnv(lookup func(string) (string, bool)) {
	set := func(key string, apply func(string)) {
		if v, ok := lookup(key); ok {
			apply(v)
		}
	}
	set("MQTT_LINAK_BROKER", func(v string) { c.MQTT.Broker = v })
	set("MQTT_LINAK_CLIENT_ID", func(v string) { c.MQTT.ClientID = v })
	set("MQTT_LINAK_USERNAME", func(v string) { c.MQTT.Username = v })
	set("MQTT_LINAK_PASSWORD", func(v string) { c.MQTT.Password = v })
	set("MQTT_LINAK_COMMAND_TOPIC_BASE", func(v string) { c.MQTT.CommandTopicBase = v })
	set("MQTT_LINAK_METRIC_TOPIC_BASE", func(v string) { c.MQTT.MetricTopicBase = v })
	set("MQTT_LINAK_HELPER_PATH", func(v string) { c.Bluetooth.HelperPath = v })
	set("MQTT_LINAK_QOS", func(v string) {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 2 {
			c.MQTT.QoS = byte(n)
		}
	})
	set("MQTT_LINAK_RETAIN", func(v string) {
		if b, err := strconv.ParseBool(v); err == nil {
			c.MQTT.Retain = b
		}
	})
}

// Validate reports the first problem that would stop the bridge starting.
func (c *Config) Validate() error {
	if strings.TrimSpace(c.MQTT.Broker) == "" {
		return fmt.Errorf("mqtt.broker is required (e.g. tcp://localhost:1883)")
	}
	if c.MQTT.QoS > 2 {
		return fmt.Errorf("mqtt.qos must be 0, 1 or 2, got %d", c.MQTT.QoS)
	}
	if err := validTopicBase("mqtt.command_topic_base", c.MQTT.CommandTopicBase); err != nil {
		return err
	}
	if err := validTopicBase("mqtt.metric_topic_base", c.MQTT.MetricTopicBase); err != nil {
		return err
	}
	if len(c.Desks) == 0 {
		return fmt.Errorf("at least one desk must be configured under desks:")
	}

	seen := make(map[string]struct{}, len(c.Desks))
	for i, d := range c.Desks {
		if err := validDeskID(d.ID); err != nil {
			return fmt.Errorf("desks[%d]: %w", i, err)
		}
		if _, dup := seen[d.ID]; dup {
			return fmt.Errorf("desks[%d]: duplicate desk id %q", i, d.ID)
		}
		seen[d.ID] = struct{}{}

		matchers := 0
		for _, v := range []string{d.Name, d.NamePrefix, d.PeripheralID} {
			if strings.TrimSpace(v) != "" {
				matchers++
			}
		}
		switch {
		case matchers == 0:
			return fmt.Errorf("desks[%d] (%s): set one of name:, name_prefix: or peripheral_id: so the desk can be recognised while scanning (run `mqtt-linak -scan` to see what is nearby)", i, d.ID)
		case matchers > 1:
			return fmt.Errorf("desks[%d] (%s): set only one of name:, name_prefix: or peripheral_id:", i, d.ID)
		}
	}

	if c.Bluetooth.ReconnectMinBackoff.D() <= 0 {
		return fmt.Errorf("bluetooth.reconnect_min_backoff must be positive")
	}
	if c.Bluetooth.ReconnectMaxBackoff.D() < c.Bluetooth.ReconnectMinBackoff.D() {
		return fmt.Errorf("bluetooth.reconnect_max_backoff must not be smaller than reconnect_min_backoff")
	}
	if c.Bluetooth.ConnectTimeout.D() <= 0 {
		return fmt.Errorf("bluetooth.connect_timeout must be positive")
	}
	return nil
}

func validTopicBase(field, v string) error {
	switch {
	case strings.TrimSpace(v) == "":
		return fmt.Errorf("%s must not be empty", field)
	case strings.ContainsAny(v, "+#"):
		return fmt.Errorf("%s must not contain MQTT wildcards (+ or #)", field)
	case strings.HasSuffix(v, "/"):
		return fmt.Errorf("%s must not end with a slash", field)
	}
	return nil
}

func validDeskID(id string) error {
	switch {
	case strings.TrimSpace(id) == "":
		return fmt.Errorf("id is required")
	case id != strings.TrimSpace(id):
		return fmt.Errorf("id %q must not have leading or trailing whitespace", id)
	case strings.ContainsAny(id, "/+#"):
		return fmt.Errorf("id %q must not contain /, + or #", id)
	case id == ReservedDeskID:
		return fmt.Errorf("id %q is reserved for the bridge's own availability topic", id)
	}
	return nil
}

// LoadBluetooth reads only the bluetooth section, for `-scan` mode where
// no broker and no desks are needed yet. A missing file is not an error:
// the defaults are enough to run a scan.
func LoadBluetooth(path string) (Bluetooth, error) {
	bt := DefaultConfig().Bluetooth

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return bt, nil
		}
		return bt, fmt.Errorf("reading config %s: %w", path, err)
	}

	var doc struct {
		Bluetooth Bluetooth `yaml:"bluetooth"`
	}
	doc.Bluetooth = bt
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return bt, fmt.Errorf("parsing config: %w", err)
	}

	var probe struct {
		Bluetooth struct {
			ScanServiceUUIDs *[]string `yaml:"scan_service_uuids"`
		} `yaml:"bluetooth"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return bt, fmt.Errorf("parsing config: %w", err)
	}
	doc.Bluetooth.ScanServiceUUIDs = nil
	if probe.Bluetooth.ScanServiceUUIDs != nil {
		doc.Bluetooth.ScanServiceUUIDs = *probe.Bluetooth.ScanServiceUUIDs
	}

	cfg := Config{Bluetooth: doc.Bluetooth}
	cfg.ApplyEnv(os.LookupEnv)
	return cfg.Bluetooth, nil
}
