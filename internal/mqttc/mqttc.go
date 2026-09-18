// Package mqttc wraps the paho MQTT client with the few behaviours this
// bridge needs: a last-will availability topic, a connect/reconnect loop
// that reports why each attempt failed, and re-subscription of every
// handler after a reconnect (a clean session starts with none).
//
// The reconnect loop is deliberately ours rather than paho's. paho's
// AutoReconnect and ConnectRetry both swallow the reason an attempt
// failed: the Connect token never completes on failure and the cause -
// bad credentials, a refused dial - goes only to paho's DEBUG logger. A
// bridge that silently never connects is the worst possible failure mode
// here, so Run owns the loop and logs every refusal.
package mqttc

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"github.com/gomi-source/mqtt-linak/internal/config"
)

// Payload values published on the availability topics.
const (
	Online  = "online"
	Offline = "offline"
)

// Reconnect pacing for Run's own connect loop.
const (
	minBackoff = 2 * time.Second
	maxBackoff = 60 * time.Second
)

// Handler receives one message for a subscribed topic filter.
type Handler func(topic string, payload []byte)

type subscription struct {
	filter  string
	handler Handler
}

// Client is a connected (or reconnecting) MQTT client.
type Client struct {
	c              mqtt.Client
	broker         string
	clientID       string
	username       string
	connectTimeout time.Duration
	qos            byte
	retain         bool
	log            *slog.Logger

	// lost is poked by paho's connection-lost handler so Run stops
	// waiting and reconnects.
	lost chan struct{}

	mu   sync.Mutex
	subs []subscription

	// onReconnect runs after every successful (re)connect, once
	// subscriptions have been restored. Used to republish retained
	// availability state that a broker restart would have lost.
	onReconnect func()
}

// Options configures New.
type Options struct {
	Config config.MQTT
	Logger *slog.Logger
	// WillTopic is published (retained) with "offline" by the broker if
	// this client disappears without a clean disconnect.
	WillTopic string
	// OnReconnect runs after each successful connect, after subscriptions
	// have been re-established.
	OnReconnect func()
}

// New builds a client. It does not connect: call Run for that, which owns
// connecting and reconnecting for the client's lifetime.
func New(ctx context.Context, opts Options) (*Client, error) {
	cfg := opts.Config
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}

	// paho keeps its own package-level loggers, all silent by default.
	// Without this, a refused connection, a rejected client ID or a
	// not-authorized CONNACK never reaches our log at all - the client
	// just quietly never connects.
	wirePahoLogging(log, cfg.Debug)

	connectTimeout := cfg.ConnectTimeout.D()
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}

	c := &Client{
		broker:         cfg.Broker,
		clientID:       cfg.ClientID,
		username:       cfg.Username,
		connectTimeout: connectTimeout,
		qos:            cfg.QoS,
		retain:         cfg.Retain,
		log:            log,
		lost:           make(chan struct{}, 1),
		onReconnect:    opts.OnReconnect,
	}

	po := mqtt.NewClientOptions().
		AddBroker(cfg.Broker).
		SetClientID(cfg.ClientID).
		SetUsername(cfg.Username).
		SetPassword(cfg.Password).
		SetKeepAlive(cfg.KeepAlive.D()).
		SetConnectTimeout(connectTimeout).
		// Both of paho's own retry mechanisms are off deliberately. With
		// either enabled, Connect's token never completes on failure and
		// the reason - "Not Authorized", a refused dial - is written only
		// to paho's DEBUG logger, so the client sits there silently never
		// connecting. Run does the retrying instead, where every failure
		// comes back as an error worth logging.
		SetAutoReconnect(false).
		SetConnectRetry(false).
		SetCleanSession(true).
		SetOrderMatters(false)

	if opts.WillTopic != "" {
		po.SetWill(opts.WillTopic, Offline, cfg.QoS, true)
	}

	po.SetOnConnectHandler(func(mqtt.Client) {
		log.Info("mqtt connected", "broker", cfg.Broker, "client_id", cfg.ClientID)
		// A clean session starts with no subscriptions, so they are
		// (re)made here on every connect rather than once at startup.
		c.resubscribe()
		if c.onReconnect != nil {
			c.onReconnect()
		}
	})
	po.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		log.Warn("mqtt connection lost, reconnecting", "err", err)
		select {
		case c.lost <- struct{}{}:
		default:
		}
	})

	c.c = mqtt.NewClient(po)
	return c, nil
}

// Run connects and keeps the connection up until ctx is done. Every
// failed attempt is logged with its reason and retried with backoff.
func (c *Client) Run(ctx context.Context) {
	backoff := minBackoff

	c.log.Info("connecting to mqtt broker",
		"broker", c.broker, "client_id", c.clientID, "username", c.usernameForLog())

	for ctx.Err() == nil {
		// Discard a drop reported for the previous connection.
		select {
		case <-c.lost:
		default:
		}

		if err := c.connectOnce(); err != nil {
			c.log.Error("mqtt connect failed",
				"broker", c.broker,
				"username", c.usernameForLog(),
				"err", err,
				"retry_in", backoff.Round(time.Millisecond))
			if isAuthError(err) {
				c.log.Error("the broker rejected these credentials; " +
					"set MQTT_LINAK_USERNAME and MQTT_LINAK_PASSWORD, or mqtt.username/password in the config")
			}
		} else {
			// Connected. OnConnect has already resubscribed.
			backoff = minBackoff
			select {
			case <-ctx.Done():
				return
			case <-c.lost:
				continue // reconnect at once; a first failure then backs off
			}
		}

		if !sleep(ctx, backoff) {
			return
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (c *Client) connectOnce() error {
	tok := c.c.Connect()
	// paho enforces connectTimeout itself; the grace is only so a token
	// that resolves right on the boundary is still read properly.
	if !tok.WaitTimeout(c.connectTimeout + 5*time.Second) {
		return fmt.Errorf("timed out after %v", c.connectTimeout)
	}
	return tok.Error()
}

// usernameForLog keeps the username visible at startup - a missing one is
// the likeliest reason for a rejected connection - without ever touching
// the password.
func (c *Client) usernameForLog() string {
	if c.username == "" {
		return "(none)"
	}
	return c.username
}

// isAuthError recognises the CONNACK refusals that mean "your credentials
// are wrong or missing" rather than "the broker is not there".
func isAuthError(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "not authorized") ||
		strings.Contains(s, "bad user name or password") ||
		strings.Contains(s, "bad username or password")
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Subscribe registers a handler. The registration is remembered and
// (re)applied on every connect; it is also applied immediately if the
// connection is already up.
func (c *Client) Subscribe(filter string, h Handler) {
	c.mu.Lock()
	c.subs = append(c.subs, subscription{filter: filter, handler: h})
	c.mu.Unlock()

	if c.Connected() {
		c.subscribeOne(filter, h)
	}
}

func (c *Client) subscribeOne(filter string, h Handler) {
	tok := c.c.Subscribe(filter, c.qos, func(_ mqtt.Client, m mqtt.Message) {
		h(m.Topic(), m.Payload())
	})
	go func() {
		if !tok.WaitTimeout(10 * time.Second) {
			c.log.Warn("mqtt subscribe timed out", "topic", filter)
			return
		}
		if err := tok.Error(); err != nil {
			c.log.Error("mqtt subscribe failed", "topic", filter, "err", err)
			return
		}
		c.log.Info("mqtt subscribed", "topic", filter)
	}()
}

func (c *Client) resubscribe() {
	c.mu.Lock()
	subs := make([]subscription, len(c.subs))
	copy(subs, c.subs)
	c.mu.Unlock()

	for _, s := range subs {
		c.subscribeOne(s.filter, s.handler)
	}
}

// Publish sends a message using the configured QoS and retain settings.
func (c *Client) Publish(topic, payload string) {
	c.publish(topic, payload, c.retain)
}

// PublishRetained always retains, regardless of the configured default.
// Availability is state, not an event, so it is retained even when
// metrics are not.
func (c *Client) PublishRetained(topic, payload string) {
	c.publish(topic, payload, true)
}

func (c *Client) publish(topic, payload string, retain bool) {
	if !c.Connected() {
		// paho would take the message and drop it. Say so instead.
		c.log.Debug("not connected, dropping message", "topic", topic)
		return
	}

	tok := c.c.Publish(topic, c.qos, retain, payload)
	if c.qos == 0 {
		// QoS 0 tokens complete as soon as the message is written; not
		// worth a goroutine per publish to observe.
		return
	}
	go func() {
		if !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
			c.log.Warn("mqtt publish failed", "topic", topic, "err", tok.Error())
		}
	}()
}

// Close publishes nothing itself; publish any final availability state
// before calling it.
func (c *Client) Close() {
	c.c.Disconnect(500)
}

// Connected reports whether the socket to the broker is actually up.
//
// This deliberately uses IsConnectionOpen rather than IsConnected: with
// paho's own retry mechanisms enabled, IsConnected also returns true
// while it is merely *trying* to connect, which makes every publish and
// subscribe issued on the strength of it fail.
func (c *Client) Connected() bool { return c.c.IsConnectionOpen() }

// ---------------------------------------------------------------------
// paho logging
// ---------------------------------------------------------------------

var pahoLoggingOnce sync.Once

// wirePahoLogging routes paho's package-level loggers into ours. They are
// global, hence the Once.
//
// Its errors and warnings are always wired: a failed connection attempt
// reaches us only through them, and that is the whole point. Its DEBUG
// stream is gated separately, because it logs every keepalive ping and
// inbound packet and buries everything else at -log-level debug.
func wirePahoLogging(log *slog.Logger, debug bool) {
	pahoLoggingOnce.Do(func() {
		l := log.With("source", "paho")
		// paho reports a failed connection attempt on ERROR; surfacing it
		// at warn keeps it visible at the default log level.
		mqtt.CRITICAL = pahoLogger{log: l, level: slog.LevelError}
		mqtt.ERROR = pahoLogger{log: l, level: slog.LevelWarn}
		mqtt.WARN = pahoLogger{log: l, level: slog.LevelDebug}
		if debug {
			mqtt.DEBUG = pahoLogger{log: l, level: slog.LevelDebug}
		}
	})
}

// pahoLogger adapts slog to paho's Logger interface.
type pahoLogger struct {
	log   *slog.Logger
	level slog.Level
}

func (p pahoLogger) Println(v ...any) {
	p.log.Log(context.Background(), p.level, strings.TrimSpace(fmt.Sprintln(v...)))
}

func (p pahoLogger) Printf(format string, v ...any) {
	p.log.Log(context.Background(), p.level, strings.TrimSpace(fmt.Sprintf(format, v...)))
}
