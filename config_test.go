package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(writeTempConfig(t, "Server:\n  token: abc\n"))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Server.Listen != ":8080" {
		t.Errorf("Listen = %q, want default :8080", cfg.Server.Listen)
	}
	if cfg.Server.Token != "abc" {
		t.Errorf("Token = %q, want abc", cfg.Server.Token)
	}
	if cfg.Connection.ChannelTransmissions != 3 {
		t.Errorf("ChannelTransmissions = %d, want default 3", cfg.Connection.ChannelTransmissions)
	}
}

func TestLoadConfigKeepsExplicitValues(t *testing.T) {
	cfg, err := loadConfig(writeTempConfig(t, "Server:\n  listen: \":9090\"\nConnection:\n  channel_transmissions: 7\n"))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Server.Listen != ":9090" {
		t.Errorf("Listen = %q, want :9090", cfg.Server.Listen)
	}
	if cfg.Connection.ChannelTransmissions != 7 {
		t.Errorf("ChannelTransmissions = %d, want 7", cfg.Connection.ChannelTransmissions)
	}
}

func TestLoadConfigNormalizesNonPositiveTransmissions(t *testing.T) {
	for _, raw := range []string{"0", "-2"} {
		cfg, err := loadConfig(writeTempConfig(t, "Connection:\n  channel_transmissions: "+raw+"\n"))
		if err != nil {
			t.Fatalf("loadConfig(%s): %v", raw, err)
		}
		if cfg.Connection.ChannelTransmissions != 3 {
			t.Errorf("channel_transmissions %s normalized to %d, want 3", raw, cfg.Connection.ChannelTransmissions)
		}
	}
}

func TestLoadConfigIgnoresUnknownSections(t *testing.T) {
	// A combined family config.yaml (bot, voice bridge sections) must parse here.
	cfg, err := loadConfig(writeTempConfig(t, "Bot:\n  commands: true\nConnection:\n  type: serial\n"))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Connection.Type != "serial" {
		t.Errorf("Type = %q, want serial", cfg.Connection.Type)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("missing config file must be an error")
	}
}

func TestLoadConfigInvalidYAML(t *testing.T) {
	if _, err := loadConfig(writeTempConfig(t, "Server: [unclosed")); err == nil {
		t.Fatal("invalid YAML must be an error")
	}
}
