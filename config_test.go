package main

import (
	"os"
	"path/filepath"
	"reflect"
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
	if !reflect.DeepEqual(cfg, DefaultConfig()) {
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
	  "tunnel-path": "/custom-tunnel",
	  "max-concurrent-streams": 128,
	  "max-request-bytes": 1048576,
	  "max-header-bytes": 65536
	}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	want := Config{
		BackoffMin:           2 * time.Second,
		BackoffMax:           45 * time.Second,
		KeepAliveInterval:    10 * time.Second,
		StreamOpenTimeout:    5 * time.Minute,
		ReadHeaderTimeout:    15 * time.Second,
		IdleTimeout:          3 * time.Minute,
		TunnelPath:           "/custom-tunnel",
		MaxConcurrentStreams: 128,
		MaxRequestBytes:      1048576,
		MaxHeaderBytes:       65536,
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("LoadConfig() = %+v, want %+v", cfg, want)
	}
}

// The optional security knobs (tunnel CN pin, device path allowlist) parse
// from their JSON keys and default to zero/empty when omitted.
func TestLoadConfigSecurityKnobs(t *testing.T) {
	path := writeConfig(t, `{"tunnel-cn": "home-mac", "allowed-path-prefixes": ["/api", "/event"]}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.TunnelCN != "home-mac" {
		t.Errorf("TunnelCN = %q, want %q", cfg.TunnelCN, "home-mac")
	}
	if !reflect.DeepEqual(cfg.AllowedPathPrefixes, []string{"/api", "/event"}) {
		t.Errorf("AllowedPathPrefixes = %v, want [/api /event]", cfg.AllowedPathPrefixes)
	}
	if def := DefaultConfig(); def.TunnelCN != "" || def.AllowedPathPrefixes != nil {
		t.Errorf("defaults should leave the security knobs empty, got %q %v", def.TunnelCN, def.AllowedPathPrefixes)
	}
}

// max-stream-duration and max-streams-per-cert both take 0 to mean "off", so
// zero must load fine while a real value round-trips.
func TestLoadConfigStreamBounds(t *testing.T) {
	path := writeConfig(t, `{"max-stream-duration": "1h", "max-streams-per-cert": 64}`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.MaxStreamDuration != time.Hour {
		t.Errorf("MaxStreamDuration = %s, want 1h", cfg.MaxStreamDuration)
	}
	if cfg.MaxStreamsPerCert != 64 {
		t.Errorf("MaxStreamsPerCert = %d, want 64", cfg.MaxStreamsPerCert)
	}
	if def := DefaultConfig(); def.MaxStreamDuration != 0 || def.MaxStreamsPerCert != 0 {
		t.Errorf("defaults should leave the stream bounds off, got %s %d", def.MaxStreamDuration, def.MaxStreamsPerCert)
	}
	// Zero is a valid, explicit "unbounded" — it must not be rejected as the
	// non-positive durations/caps are.
	if _, err := LoadConfig(writeConfig(t, `{"max-stream-duration": "0s", "max-streams-per-cert": 0}`)); err != nil {
		t.Fatalf("LoadConfig() error = %v, want nil for explicit zero bounds", err)
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
	if !reflect.DeepEqual(cfg, want) {
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
		{"zero concurrency", `{"max-concurrent-streams": 0}`, "max-concurrent-streams"},
		{"negative body cap", `{"max-request-bytes": -1}`, "max-request-bytes"},
		{"zero header cap", `{"max-header-bytes": 0}`, "max-header-bytes"},
		{"path prefix missing leading slash", `{"allowed-path-prefixes": ["api"]}`, "allowed-path-prefixes"},
		{"negative stream duration", `{"max-stream-duration": "-1s"}`, "max-stream-duration"},
		{"negative per-cert cap", `{"max-streams-per-cert": -1}`, "max-streams-per-cert"},
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
	if !reflect.DeepEqual(cfg, Config{}) {
		t.Errorf("LoadConfig() = %+v on error, want the zero Config", cfg)
	}
}
