// meshcore-http-gateway runs a MeshCore LoRa device as a thin HTTP gateway:
// everything the radio receives accumulates in an unread inbox exposed via
// GET /get (and the cursor-based /messages), and external clients send mesh
// messages via POST /send. See server.go for the API and README.md for docs.
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
	"syscall"
	"time"

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

	// Meshcore client — BLE (default), USB serial or TCP (Wi-Fi/Ethernet)
	// depending on config.
	client, err := companion.Connect(companion.ConnectOptions{
		Type:          cfg.Connection.Type,
		SerialPort:    cfg.Connection.SerialPort,
		SerialBaud:    cfg.Connection.SerialBaud,
		TCPAddress:    cfg.Connection.TCPAddress,
		DeviceName:    cfg.Connection.DeviceName,
		DeviceAddress: cfg.Connection.DeviceAddress,
	})
	if err != nil {
		log.Error("Failed to connect to device", "error", err)
		os.Exit(1)
	}
	defer client.Disconnect()

	// Device runtime: startup stored-message sync, live forwarding into the
	// inbox, keepalive/reconnect loop.
	svc := companion.NewService(client, log)
	svc.Start(ctx)

	// ─── HTTP server ──────────────────────────────────────────────────────────

	listen := cfg.Server.Listen
	if listen == "" {
		listen = ":8080"
	}

	srv := &Server{
		client: client,
		inbox:  svc.Inbox,
		opts: Options{
			Token:                cfg.Server.Token,
			ChannelTransmissions: cfg.Connection.ChannelTransmissions,
		},
		log: log,
	}

	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		// Blocking sends wait through device confirmations and retry pacing —
		// tens of seconds worst case — so writes get a generous ceiling.
		WriteTimeout: 90 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.ListenAndServe() }()

	log.Info("HTTP gateway listening", "addr", listen, "auth", cfg.Server.Token != "")
	log.Info("Endpoints: GET /get, GET /messages, GET /contacts, GET /channels, POST /send")

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
