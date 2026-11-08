package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// config is the gateway's own config.yaml — a subset of the family format:
// the Server section (listen/token) and the Connection section (transport).
// Unknown sections are ignored, so a combined config.yaml shared with other
// family apps keeps working.
type config struct {
	Server struct {
		Listen string `yaml:"listen"` // bind address; default ":8080"
		Token  string `yaml:"token"`  // optional bearer auth; empty = no auth
	} `yaml:"Server"`

	Connection struct {
		// Type selects the transport: "bt" (Bluetooth LE, default), "serial"
		// (USB/UART) or "tcp" (alias "wifi": Wi-Fi/Ethernet companion firmware).
		Type string `yaml:"type"`
		// SerialPort is the OS device path used when Type is "serial".
		SerialPort string `yaml:"serial_port"`
		// SerialBaud sets the baud rate for the serial transport (default 115200).
		SerialBaud int `yaml:"serial_baud"`
		// TCPAddress is the device address used when Type is "tcp" — the Wi-Fi
		// or Ethernet companion firmware serves the same protocol on TCP.
		// Either "host:port" or a bare host, which gets the default port 5000.
		TCPAddress string `yaml:"tcp_address"`
		// DeviceName selects the BLE device by exact advertised name (e.g.
		// "MeshCore-AB12") when Type is "bt". Empty scans any MeshCore device
		// and picks the strongest signal. The portable identifier on macOS,
		// where the address is a per-machine CoreBluetooth UUID.
		DeviceName string `yaml:"device_name"`
		// DeviceAddress selects the BLE device by platform address when Type
		// is "bt" (MAC on Linux/Windows, CoreBluetooth UUID on macOS).
		// Combine with DeviceName or use alone.
		DeviceAddress string `yaml:"device_address"`
		// ChannelTransmissions is the MAXIMUM number of times each channel
		// message is transmitted (deduplicated by receivers) to improve
		// delivery against silent RF drops. 0 or unset defaults to 3.
		ChannelTransmissions int `yaml:"channel_transmissions"`
	} `yaml:"Connection"`

	Push struct {
		// Enabled turns on push mode: a background worker POSTs every new
		// message to EndpointURL as it arrives (see push.go). Default false.
		Enabled bool `yaml:"enabled"`
		// EndpointURL is where new messages are pushed — an http:// or
		// https:// URL that accepts one POST per message (the same JSON as
		// the /get response's per-message object). Required when Enabled.
		EndpointURL string `yaml:"endpoint_url"`
		// Token is an optional bearer token sent to the endpoint as
		// "Authorization: Bearer <token>". Empty = no header.
		Token string `yaml:"token"`
		// TimeoutSeconds bounds each push POST. 0 or unset defaults to 10.
		TimeoutSeconds int `yaml:"timeout_seconds"`
	} `yaml:"Push"`
}

// loadConfig reads and parses the config file, then fills in the documented
// defaults. A missing file is an error: without connection settings there is
// nothing useful to serve.
func loadConfig(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	var c config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	c.normalize()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// validate rejects Push settings that would fail on every delivery anyway,
// so a misconfigured push section stops the process at startup instead of
// silently delivering nothing.
func (c *config) validate() error {
	if !c.Push.Enabled {
		return nil
	}
	if c.Push.EndpointURL == "" {
		return errors.New("Push.enabled is set but Push.endpoint_url is empty")
	}
	if !strings.HasPrefix(c.Push.EndpointURL, "http://") && !strings.HasPrefix(c.Push.EndpointURL, "https://") {
		return fmt.Errorf("Push.endpoint_url %q must start with http:// or https://", c.Push.EndpointURL)
	}
	return nil
}

// normalize fills defaults into fields the YAML left empty, so callers never
// see unset values.
func (c *config) normalize() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Connection.ChannelTransmissions <= 0 {
		c.Connection.ChannelTransmissions = 3
	}
	if c.Push.TimeoutSeconds <= 0 {
		c.Push.TimeoutSeconds = 10
	}
}
