package main

import (
	"fmt"
	"os"

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
		// Type selects the transport: "bt" (Bluetooth LE, default) or "serial" (USB/UART).
		Type string `yaml:"type"`
		// SerialPort is the OS device path used when Type is "serial".
		SerialPort string `yaml:"serial_port"`
		// SerialBaud sets the baud rate for the serial transport (default 115200).
		SerialBaud int `yaml:"serial_baud"`
		// ChannelTransmissions is the MAXIMUM number of times each channel
		// message is transmitted (deduplicated by receivers) to improve
		// delivery against silent RF drops. 0 or unset defaults to 3.
		ChannelTransmissions int `yaml:"channel_transmissions"`
	} `yaml:"Connection"`
}

// loadConfig reads and parses the config file. A missing file is an error:
// without connection settings there is nothing useful to serve.
func loadConfig(path string) (*config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}
	var c config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	return &c, nil
}
