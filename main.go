// meshcore-http-gateway runs a MeshCore LoRa device as a thin HTTP gateway:
// everything the radio receives accumulates in an unread inbox exposed via
// GET /get (and the cursor-based /messages), external clients send mesh
// messages via POST /send, device-level CLI commands go through POST /cli,
// and GET /status serves a cached device snapshot. Optionally the inbox is
// also pushed — every new message POSTed to a configured webhook endpoint
// (push.go). See server.go for the API and README.md for docs.
//
// It is a standalone feature with its own device connection — independent of
// the bot and the voice bridge. Don't run it against the same physical device
// as another family app at the same time: a companion device serves one
// connection.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/stempit/meshcore-go"
	"github.com/stempit/meshcore-kit/companion"
)

func main() {
	cfg, err := loadConfig("config.yaml")
	if err != nil {
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	log := slog.Default()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client, err := connectDevice(cfg)
	if err != nil {
		log.Error("Failed to connect to device", "error", err)
		os.Exit(1)
	}
	defer client.Disconnect()

	// Sample the device clock while the wire is quiet — before the stored
	// message sync starts: GET /status serves the reading with its sample
	// time. Failure is logged, not fatal: /status just omits it.
	clock := sampleDeviceTime(client, log)

	// Device runtime: startup stored-message sync, live forwarding into the
	// inbox, keepalive/reconnect loop.
	svc := companion.NewService(client, log)
	svc.Start(ctx)

	// Push mode: POST every new message to the configured webhook endpoint.
	// Starts after svc.Start, so the stored-message sync is not re-pushed.
	if cfg.Push.Enabled {
		p := newPusher(cfg, svc.Inbox, log)
		go p.run(ctx)
		log.Info("Push enabled", "endpoint", cfg.Push.EndpointURL, "auth", cfg.Push.Token != "")
	}

	httpSrv := newHTTPServer(cfg, client, svc.Inbox, clock, log)

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.ListenAndServe() }()

	log.Info("HTTP gateway listening", "addr", cfg.Server.Listen, "auth", cfg.Server.Token != "")
	log.Info("Endpoints: GET /get, GET /messages, GET /contacts, GET /channels, GET /status, POST /send, POST /cli")

	select {
	case <-ctx.Done():
		log.Info("Shutting down...")
	case err := <-serveErr:
		log.Error("HTTP server failed", "error", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Error("HTTP server shutdown error", "error", err)
	}
}

// connectDevice opens the device connection selected by the Connection
// config: BLE (default), USB serial or TCP (Wi-Fi/Ethernet).
func connectDevice(cfg *config) (*meshcore.MeshCoreClient, error) {
	return companion.Connect(companion.ConnectOptions{
		Type:          cfg.Connection.Type,
		SerialPort:    cfg.Connection.SerialPort,
		SerialBaud:    cfg.Connection.SerialBaud,
		TCPAddress:    cfg.Connection.TCPAddress,
		DeviceName:    cfg.Connection.DeviceName,
		DeviceAddress: cfg.Connection.DeviceAddress,
	})
}

// sampleDeviceTime reads the device's RTC once via GetDeviceTime. It runs
// before svc.Start so the query does not interleave with the stored-message
// sync traffic; a failure leaves the zero clock, which /status omits.
func sampleDeviceTime(client *meshcore.MeshCoreClient, log *slog.Logger) startupClock {
	at := time.Now()
	ts, err := client.GetDeviceTime()
	if err != nil {
		log.Warn("Failed to sample device time; /status will omit it", "error", err)
		return startupClock{}
	}
	return startupClock{DeviceTime: ts, SampledAt: at}
}

// normalizeTransport maps the configured connection type to the canonical
// transport name GET /status reports: empty means the default BLE, and
// "wifi" is an alias of "tcp" (both accepted by companion.Connect).
func normalizeTransport(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == "" {
		return "bt"
	}
	if t == "wifi" {
		return "tcp"
	}
	return t
}

// newHTTPServer wires the gateway API onto the device client and inbox.
func newHTTPServer(cfg *config, client *meshcore.MeshCoreClient, inbox *companion.Inbox, clock startupClock, log *slog.Logger) *http.Server {
	srv := &Server{
		client: client,
		inbox:  inbox,
		opts: Options{
			Token:                cfg.Server.Token,
			ChannelTransmissions: cfg.Connection.ChannelTransmissions,
			Transport:            normalizeTransport(cfg.Connection.Type),
		},
		startedAt: time.Now(),
		clock:     clock,
		log:       log,
	}
	return &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// Blocking sends wait through device confirmations and retry pacing —
		// tens of seconds worst case — so writes get a generous ceiling.
		WriteTimeout: 90 * time.Second,
		// Reap idle keep-alive connections; handler time is bounded by
		// WriteTimeout, so this only needs to cover the between-requests gap.
		IdleTimeout: 120 * time.Second,
	}
}
