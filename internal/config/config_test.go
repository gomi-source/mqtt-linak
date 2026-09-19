package config

import (
	"strings"
	"testing"
	"time"
)

const minimal = `
mqtt:
  broker: mqtt://localhost:1883
desks:
  - id: office
    name: "Desk 8802"
`

func TestParseMinimalAppliesDefaults(t *testing.T) {
	cfg, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.MQTT.ClientID != "mqtt-linak" {
		t.Errorf("client_id = %q, want the default", cfg.MQTT.ClientID)
	}
	if cfg.MQTT.CommandTopicBase != "cmd/linak" || cfg.MQTT.MetricTopicBase != "tele/linak" {
		t.Errorf("topic bases = %q/%q, want the defaults", cfg.MQTT.CommandTopicBase, cfg.MQTT.MetricTopicBase)
	}
	if !cfg.MQTT.Retain {
		t.Error("retain should default to true")
	}
	if cfg.Bluetooth.ReconnectMaxBackoff.D() != time.Minute {
		t.Errorf("reconnect_max_backoff = %v, want 1m", cfg.Bluetooth.ReconnectMaxBackoff.D())
	}
	// A DPG controller drops an idle connection after four hours, so the
	// default has to leave room for several attempts before that.
	if ka := cfg.Bluetooth.KeepAliveInterval.D(); ka <= 0 || ka >= 4*time.Hour {
		t.Errorf("keepalive_interval = %v, want a positive interval well under the desk's four-hour idle timeout", ka)
	}
	// nil means unfiltered: a LINAK controller advertises no service to
	// filter on.
	if cfg.Bluetooth.ScanServiceUUIDs != nil {
		t.Errorf("scan_service_uuids = %v, want nil when unset", cfg.Bluetooth.ScanServiceUUIDs)
	}
}

func TestParseDurationsAndExplicitEmptyScanList(t *testing.T) {
	cfg, err := Parse([]byte(`
mqtt:
  broker: mqtt://broker:1883
bluetooth:
  connect_timeout: 45s
  publish_min_interval: 1s
  scan_service_uuids: []
desks:
  - id: a
    peripheral_id: 1234ABCD-0000-0000-0000-000000000000
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.Bluetooth.ConnectTimeout.D(); got != 45*time.Second {
		t.Errorf("connect_timeout = %v, want 45s", got)
	}
	if got := cfg.Bluetooth.PublishMinInterval.D(); got != time.Second {
		t.Errorf("publish_min_interval = %v, want 1s", got)
	}
	if cfg.Bluetooth.ScanServiceUUIDs == nil || len(cfg.Bluetooth.ScanServiceUUIDs) != 0 {
		t.Errorf("an explicit empty list should stay empty, got %v", cfg.Bluetooth.ScanServiceUUIDs)
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	_, err := Parse([]byte(minimal + "\nnonsense: 1\n"))
	if err == nil {
		t.Fatal("want an error for an unknown top-level key")
	}
}

func TestParseRejectsBadDuration(t *testing.T) {
	_, err := Parse([]byte(`
mqtt:
  broker: mqtt://b:1883
bluetooth:
  connect_timeout: soon
desks:
  - id: a
    name: b
`))
	if err == nil || !strings.Contains(err.Error(), "invalid duration") {
		t.Fatalf("want an invalid-duration error, got %v", err)
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MQTT.Broker = "mqtt://from-file:1883"
	cfg.ApplyEnv(func(k string) (string, bool) {
		switch k {
		case "MQTT_LINAK_BROKER":
			return "mqtt://from-env:1883", true
		case "MQTT_LINAK_PASSWORD":
			return "s3cret", true
		case "MQTT_LINAK_QOS":
			return "2", true
		case "MQTT_LINAK_RETAIN":
			return "false", true
		}
		return "", false
	})
	if cfg.MQTT.Broker != "mqtt://from-env:1883" {
		t.Errorf("broker = %q, want the env value", cfg.MQTT.Broker)
	}
	if cfg.MQTT.Password != "s3cret" {
		t.Errorf("password = %q", cfg.MQTT.Password)
	}
	if cfg.MQTT.QoS != 2 {
		t.Errorf("qos = %d, want 2", cfg.MQTT.QoS)
	}
	if cfg.MQTT.Retain {
		t.Error("retain should have been turned off by the env override")
	}
}

func TestApplyEnvIgnoresGarbage(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ApplyEnv(func(k string) (string, bool) {
		if k == "MQTT_LINAK_QOS" {
			return "9", true
		}
		return "", false
	})
	if cfg.MQTT.QoS != 0 {
		t.Errorf("qos = %d, an out-of-range override should be ignored", cfg.MQTT.QoS)
	}
}

func TestValidate(t *testing.T) {
	base := func() Config {
		c := DefaultConfig()
		c.MQTT.Broker = "mqtt://b:1883"
		c.Desks = []Desk{{ID: "office", Name: "Desk"}}
		return c
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"ok", func(*Config) {}, ""},
		{"no broker", func(c *Config) { c.MQTT.Broker = " " }, "mqtt.broker is required"},
		{"no desks", func(c *Config) { c.Desks = nil }, "at least one desk"},
		{"blank id", func(c *Config) { c.Desks[0].ID = "" }, "id is required"},
		{"slash in id", func(c *Config) { c.Desks[0].ID = "a/b" }, "must not contain"},
		{"wildcard in id", func(c *Config) { c.Desks[0].ID = "a+b" }, "must not contain"},
		{"reserved id", func(c *Config) { c.Desks[0].ID = "bridge" }, "reserved"},
		{"duplicate id", func(c *Config) {
			c.Desks = append(c.Desks, Desk{ID: "office", Name: "Other"})
		}, "duplicate desk id"},
		{"no matcher", func(c *Config) { c.Desks[0].Name = "" }, "set one of name:"},
		{"two matchers", func(c *Config) { c.Desks[0].PeripheralID = "x" }, "only one of"},
		{"three matchers", func(c *Config) {
			c.Desks[0].NamePrefix = "Desk"
			c.Desks[0].PeripheralID = "x"
		}, "only one of"},
		{"name_prefix alone is fine", func(c *Config) {
			c.Desks[0].Name = ""
			c.Desks[0].NamePrefix = "Desk"
		}, ""},
		{"wildcard topic base", func(c *Config) { c.MQTT.CommandTopicBase = "linak/+" }, "wildcards"},
		{"trailing slash", func(c *Config) { c.MQTT.MetricTopicBase = "linak/" }, "must not end with a slash"},
		{"bad qos", func(c *Config) { c.MQTT.QoS = 5 }, "mqtt.qos"},
		{"backoff inverted", func(c *Config) {
			c.Bluetooth.ReconnectMinBackoff = Duration(time.Minute)
			c.Bluetooth.ReconnectMaxBackoff = Duration(time.Second)
		}, "reconnect_max_backoff"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.mutate(&c)
			err := c.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want an error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadBluetoothTolerateMissingFile(t *testing.T) {
	bt, err := LoadBluetooth("/definitely/not/here.yaml")
	if err != nil {
		t.Fatalf("a missing file should not fail a scan: %v", err)
	}
	if bt.ConnectTimeout.D() != 30*time.Second {
		t.Errorf("connect_timeout = %v, want the default", bt.ConnectTimeout.D())
	}
	if bt.ScanServiceUUIDs != nil {
		t.Errorf("scan_service_uuids = %v, want nil (unfiltered)", bt.ScanServiceUUIDs)
	}
}

func TestParseNamePrefixDesk(t *testing.T) {
	cfg, err := Parse([]byte(`
mqtt:
  broker: mqtt://b:1883
desks:
  - id: office
    name_prefix: "Desk "
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Desks[0].NamePrefix != "Desk " {
		t.Errorf("name_prefix = %q", cfg.Desks[0].NamePrefix)
	}
}
