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
	// ReadHeaderTimeout caps how long a peer may take to send request
	// headers, so a slow-header (Slowloris) client can't pin a connection's
	// goroutine open indefinitely. It bounds only the header read, not the
	// body or response, so long-lived SSE streams are unaffected.
	ReadHeaderTimeout time.Duration
	// IdleTimeout caps how long a kept-alive connection may sit idle between
	// requests before the server closes it, reclaiming goroutines from peers
	// that connect and then go quiet.
	IdleTimeout time.Duration
	// TunnelPath is the HTTP path the remote half listens on for the tunnel
	// upgrade; every other path is treated as a device request.
	TunnelPath string
	// MaxConcurrentStreams caps how many device requests may be in flight
	// through the single tunnel at once. Each one opens a yamux stream, a
	// bounded resource; without a cap one authenticated (or stolen) device
	// cert could open requests in a loop and starve every other device. The
	// remote returns 503 once this many are already in flight.
	MaxConcurrentStreams int
	// MaxRequestBytes caps a device request body, so a single client can't
	// stream an unbounded upload through the tunnel. It bounds only the
	// request body — opencode's responses, including the long-lived SSE
	// stream, are unaffected.
	MaxRequestBytes int64
	// MaxHeaderBytes caps request header size on both halves' http.Servers,
	// making Go's implicit 1MB default explicit and tunable.
	MaxHeaderBytes int
}

func DefaultConfig() Config {
	return Config{
		BackoffMin:        time.Second,
		BackoffMax:        30 * time.Second,
		KeepAliveInterval: 30 * time.Second,
		StreamOpenTimeout: 2 * time.Minute,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		TunnelPath:        "/_tunnel",

		MaxConcurrentStreams: 256,
		MaxRequestBytes:      32 << 20, // 32 MiB
		MaxHeaderBytes:       1 << 20,  // 1 MiB, matching net/http's implicit default
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
	ReadHeaderTimeout *string `json:"read-header-timeout"`
	IdleTimeout       *string `json:"idle-timeout"`
	TunnelPath        *string `json:"tunnel-path"`

	MaxConcurrentStreams *int   `json:"max-concurrent-streams"`
	MaxRequestBytes      *int64 `json:"max-request-bytes"`
	MaxHeaderBytes       *int   `json:"max-header-bytes"`
}

// durationField pairs one of Config's time.Duration fields with its JSON
// source (nil when validate builds the list, since validate has no raw JSON
// to read from). Keeping name, text and dst together in one row — rather
// than in separate lists UnmarshalJSON and validate each had to keep in sync
// by hand, or matched positionally across parallel slices — makes it
// impossible for a field to end up validated against, or unmarshalled into,
// the wrong destination.
type durationField struct {
	name string
	text *string
	dst  *time.Duration
}

// durationFields lists c's duration fields alongside their raw JSON source.
func (c *Config) durationFields(raw configJSON) []durationField {
	return []durationField{
		{"backoff-min", raw.BackoffMin, &c.BackoffMin},
		{"backoff-max", raw.BackoffMax, &c.BackoffMax},
		{"keepalive-interval", raw.KeepAliveInterval, &c.KeepAliveInterval},
		{"stream-open-timeout", raw.StreamOpenTimeout, &c.StreamOpenTimeout},
		{"read-header-timeout", raw.ReadHeaderTimeout, &c.ReadHeaderTimeout},
		{"idle-timeout", raw.IdleTimeout, &c.IdleTimeout},
	}
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
	for _, f := range c.durationFields(raw) {
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
	if raw.MaxConcurrentStreams != nil {
		c.MaxConcurrentStreams = *raw.MaxConcurrentStreams
	}
	if raw.MaxRequestBytes != nil {
		c.MaxRequestBytes = *raw.MaxRequestBytes
	}
	if raw.MaxHeaderBytes != nil {
		c.MaxHeaderBytes = *raw.MaxHeaderBytes
	}
	return nil
}

// validate rejects values that would fail quietly rather than loudly: yamux
// reads a non-positive timeout as "disabled", not "shorter", and an inverted
// backoff range would cap every delay below the first attempt's.
func (c Config) validate() error {
	for _, f := range c.durationFields(configJSON{}) {
		if *f.dst <= 0 {
			return fmt.Errorf("%s must be positive, got %s", f.name, *f.dst)
		}
	}
	if c.BackoffMin > c.BackoffMax {
		return fmt.Errorf("backoff-min (%s) must not exceed backoff-max (%s)", c.BackoffMin, c.BackoffMax)
	}
	if !strings.HasPrefix(c.TunnelPath, "/") {
		return fmt.Errorf("tunnel-path must start with /, got %q", c.TunnelPath)
	}
	// The same failure mode as the durations: a non-positive limit reads as
	// "disabled" (an empty semaphore blocks every request, a zero byte cap
	// rejects every body), not "smaller", so reject it loudly.
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"max-concurrent-streams", int64(c.MaxConcurrentStreams)},
		{"max-request-bytes", c.MaxRequestBytes},
		{"max-header-bytes", int64(c.MaxHeaderBytes)},
	} {
		if f.v <= 0 {
			return fmt.Errorf("%s must be positive, got %d", f.name, f.v)
		}
	}
	return nil
}
