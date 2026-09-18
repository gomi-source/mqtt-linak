// Command mqtt-linak bridges LINAK DPG desk controllers, reached over
// Bluetooth LE via macOS CoreBluetooth, to an MQTT broker.
//
// It connects to every desk listed in the config file, keeps those
// connections up, republishes each desk's height as it changes, and
// applies height / base height commands published to MQTT.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gomi-source/mqtt-linak/internal/bridge"
	"github.com/gomi-source/mqtt-linak/internal/config"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
var version = "dev"

func main() {
	var (
		configPath  = flag.String("config", "config.yaml", "path to the YAML configuration file")
		logLevel    = flag.String("log-level", "info", "log level: debug, info, warn or error")
		logFormat   = flag.String("log-format", "text", "log format: text or json")
		mqttDebug   = flag.Bool("mqtt-debug", false, "log paho's own MQTT protocol trace (very chatty: every keepalive ping and packet); needs -log-level debug to be visible")
		scanOnly    = flag.Bool("scan", false, "scan for nearby Bluetooth peripherals, print what is found, and exit")
		scanFor     = flag.Duration("scan-duration", 12*time.Second, "how long -scan listens for")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("mqtt-linak", version)
		return
	}

	log, err := newLogger(*logLevel, *logFormat)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mqtt-linak:", err)
		os.Exit(2)
	}
	slog.SetDefault(log)

	if env := os.Getenv("MQTT_LINAK_CONFIG"); env != "" && isDefaultFlag("config") {
		*configPath = env
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *scanOnly {
		if err := runScan(ctx, *configPath, *scanFor, log); err != nil {
			log.Error("scan failed", "err", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("configuration error", "err", err)
		os.Exit(1)
	}

	if *mqttDebug {
		cfg.MQTT.Debug = true
	}

	log.Info("starting mqtt-linak",
		"version", version,
		"broker", cfg.MQTT.Broker,
		"desks", len(cfg.Desks),
		"command_topic_base", cfg.MQTT.CommandTopicBase,
		"metric_topic_base", cfg.MQTT.MetricTopicBase)

	if err := bridge.New(cfg, log).Run(ctx); err != nil {
		log.Error("bridge stopped", "err", err)
		os.Exit(1)
	}
	log.Info("stopped")
}

// runScan answers "what is my desk called, and what is its UUID?" — the
// question that has to be settled before a desks entry can be written,
// and which a service-filtered scan cannot answer because a LINAK
// controller does not advertise its control service.
func runScan(ctx context.Context, configPath string, d time.Duration, log *slog.Logger) error {
	bt, err := config.LoadBluetooth(configPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Scanning for %s...\n\n", d)

	sightings, err := bridge.Inventory(ctx, bt, d, log)
	if err != nil && len(sightings) == 0 {
		return err
	}
	bridge.PrintInventory(os.Stdout, sightings)
	return nil
}

func newLogger(level, format string) (*slog.Logger, error) {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		return nil, fmt.Errorf("unknown log level %q", level)
	}

	opts := &slog.HandlerOptions{Level: lvl}
	switch strings.ToLower(format) {
	case "text", "":
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, opts)), nil
	default:
		return nil, fmt.Errorf("unknown log format %q", format)
	}
}

// isDefaultFlag reports whether a flag was left at its default, so an
// environment variable can fill it in without overriding an explicit flag.
func isDefaultFlag(name string) bool {
	seen := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return !seen
}
