package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigEmptyPathReturnsDefaults(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig(\"\") error = %v, want nil", err)
	}
	if cfg != DefaultConfig() {
		t.Errorf("LoadConfig(\"\") = %+v, want %+v", cfg, DefaultConfig())
	}
}

func TestLoadConfigReadsEveryKey(t *testing.T) {
	path := writeConfig(t, `{
	  "backoff-min": "2s",
	  "backoff-max": "45s",
	  "keepalive-interval": "10s",
	  "stream-open-timeout": "5m",
	  "read-header-timeout": "15s",
	  "idle-timeout": "3m",
	  "tunnel-path": "/custom-tunnel"
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	want := Config{
		BackoffMin:        2 * time.Second,
		BackoffMax:        45 * time.Second,
		KeepAliveInterval: 10 * time.Second,
		StreamOpenTimeout: 5 * time.Minute,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       3 * time.Minute,
		TunnelPath:        "/custom-tunnel",
	}
	if cfg != want {
		t.Errorf("LoadConfig() = %+v, want %+v", cfg, want)
	}
}

// A partial file is the common case — an operator tuning one knob shouldn't
// have to restate the rest.
func TestLoadConfigKeepsDefaultsForOmittedKeys(t *testing.T) {
	path := writeConfig(t, `{"keepalive-interval": "5s"}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	want := DefaultConfig()
	want.KeepAliveInterval = 5 * time.Second
	if cfg != want {
		t.Errorf("LoadConfig() = %+v, want %+v", cfg, want)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantText string
	}{
		{"zero duration", `{"backoff-min": "0s"}`, "backoff-min"},
		{"negative duration", `{"stream-open-timeout": "-1s"}`, "stream-open-timeout"},
		{"inverted backoff range", `{"backoff-min": "60s"}`, "backoff-min"},
		{"unparseable duration", `{"backoff-max": "30 seconds"}`, "backoff-max"},
		{"duration as a number", `{"backoff-max": 30}`, "backoff-max"},
		{"unknown key", `{"backoff-minimum": "1s"}`, "backoff-minimum"},
		{"tunnel path missing leading slash", `{"tunnel-path": "tunnel"}`, "tunnel-path"},
		{"malformed json", `{`, "parse config"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadConfig(writeConfig(t, tt.body))
			if err == nil {
				t.Fatalf("LoadConfig(%s) error = nil, want an error", tt.body)
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tt.wantText)
			}
		})
	}
}

// Equal bounds are the boundary between valid and invalid: validate rejects
// BackoffMin > BackoffMax, so BackoffMin == BackoffMax must still be accepted.
func TestLoadConfigAllowsEqualBackoffBounds(t *testing.T) {
	path := writeConfig(t, `{"backoff-min": "30s", "backoff-max": "30s"}`)
	if _, err := LoadConfig(path); err != nil {
		t.Fatalf("LoadConfig() error = %v, want nil for backoff-min == backoff-max", err)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json"))
	if err == nil {
		t.Fatal("LoadConfig() error = nil, want an error for a missing file")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "read config")
	}
}

// A file that fails validation must not leave a half-applied Config behind for
// a caller that ignores the error.
func TestLoadConfigReturnsZeroConfigOnError(t *testing.T) {
	cfg, err := LoadConfig(writeConfig(t, `{"backoff-min": "0s"}`))
	if err == nil {
		t.Fatal("expected an error")
	}
	if cfg != (Config{}) {
		t.Errorf("LoadConfig() = %+v on error, want the zero Config", cfg)
	}
}
