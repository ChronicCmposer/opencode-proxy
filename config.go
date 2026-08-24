// The operational tunables that don't belong hardcoded in constructors: the
// reconnect backoff bounds and the yamux session timeouts. They're read from
// the JSON file named by --config and layered over DefaultConfig, so a file
// that sets one key leaves the rest at their defaults and omitting --config
// entirely is still valid.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type Config struct {
	// BackoffMin is the delay before the local client's first reconnect
	// attempt; each attempt after that doubles it.
	BackoffMin time.Duration
	// BackoffMax caps the reconnect delay, jitter included.
	BackoffMax time.Duration
	// KeepAliveInterval is how often an idle yamux session pings its peer, so
	// a tunnel dropped by a NAT or load balancer is noticed rather than
	// waited on.
	KeepAliveInterval time.Duration
	// StreamOpenTimeout wants to be generous: a browser's GET /event SSE
	// stream is expected to sit idle-but-open for a long time.
	StreamOpenTimeout time.Duration
	// TunnelPath is the HTTP path the remote half listens on for the tunnel
	// upgrade; every other path is treated as a device request.
	TunnelPath string
}

func DefaultConfig() Config {
	return Config{
		BackoffMin:        time.Second,
		BackoffMax:        30 * time.Second,
		KeepAliveInterval: 30 * time.Second,
		StreamOpenTimeout: 2 * time.Minute,
		TunnelPath:        "/_tunnel",
	}
}

// LoadConfig returns the defaults untouched when path is empty, so running
// without --config stays valid. Otherwise it layers path's keys over the
// defaults and validates the result, reporting the path in every error since
// the caller only ever prints what comes back.
func LoadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

// configJSON mirrors Config with duration strings: encoding/json decodes a
// time.Duration only as an integer nanosecond count, and "30s" is what belongs
// in a hand-edited file. The pointers distinguish an absent key — keep the
// default already in the Config — from one the file actually sets.
type configJSON struct {
	BackoffMin        *string `json:"backoff-min"`
	BackoffMax        *string `json:"backoff-max"`
	KeepAliveInterval *string `json:"keepalive-interval"`
	StreamOpenTimeout *string `json:"stream-open-timeout"`
	TunnelPath        *string `json:"tunnel-path"`
}

// UnmarshalJSON layers b's keys over whatever c already holds rather than
// replacing it, which is what lets LoadConfig seed c with DefaultConfig first.
// Unknown keys are an error: a typo'd key would otherwise be indistinguishable
// from an omitted one, silently leaving the default in place.
func (c *Config) UnmarshalJSON(b []byte) error {
	var raw configJSON
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		text *string
		dst  *time.Duration
	}{
		{"backoff-min", raw.BackoffMin, &c.BackoffMin},
		{"backoff-max", raw.BackoffMax, &c.BackoffMax},
		{"keepalive-interval", raw.KeepAliveInterval, &c.KeepAliveInterval},
		{"stream-open-timeout", raw.StreamOpenTimeout, &c.StreamOpenTimeout},
	} {
		if f.text == nil {
			continue
		}
		d, err := time.ParseDuration(*f.text)
		if err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
		*f.dst = d
	}
	if raw.TunnelPath != nil {
		c.TunnelPath = *raw.TunnelPath
	}
	return nil
}

// validate rejects values that would fail quietly rather than loudly: yamux
// reads a non-positive timeout as "disabled", not "shorter", and an inverted
// backoff range would cap every delay below the first attempt's.
func (c Config) validate() error {
	for _, f := range []struct {
		name string
		d    time.Duration
	}{
		{"backoff-min", c.BackoffMin},
		{"backoff-max", c.BackoffMax},
		{"keepalive-interval", c.KeepAliveInterval},
		{"stream-open-timeout", c.StreamOpenTimeout},
	} {
		if f.d <= 0 {
			return fmt.Errorf("%s must be positive, got %s", f.name, f.d)
		}
	}
	if c.BackoffMin > c.BackoffMax {
		return fmt.Errorf("backoff-min (%s) must not exceed backoff-max (%s)", c.BackoffMin, c.BackoffMax)
	}
	if !strings.HasPrefix(c.TunnelPath, "/") {
		return fmt.Errorf("tunnel-path must start with /, got %q", c.TunnelPath)
	}
	return nil
}
