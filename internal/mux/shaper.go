package mux

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Option configures a Lane created by Client or Server.
type Option func(*laneConfig)

type laneConfig struct{ rateBytesPerSec int }

// WithRate shapes the Lane's outbound DTLS emission to at most bytesPerSec, pacing
// bursts into a steadier, more call-like flow (footprint shaping). 0 = unlimited.
func WithRate(bytesPerSec int) Option {
	return func(c *laneConfig) { c.rateBytesPerSec = bytesPerSec }
}

// applyLaneOpts applies Client/Server options to a freshly created Lane (before KCP
// starts writing through it).
func applyLaneOpts(l *Lane, opts []Option) {
	var c laneConfig
	for _, o := range opts {
		o(&c)
	}
	if c.rateBytesPerSec > 0 {
		l.shaper = newShaper(c.rateBytesPerSec)
	}
}

// shaper is a token bucket that paces byte emission to a fixed average rate, so a
// bursty tunnel looks more like a constant-bitrate call on the wire.
type shaper struct {
	mu     sync.Mutex
	rate   float64 // bytes per second
	burst  float64 // max accumulated tokens (~1s)
	tokens float64
	last   time.Time
}

func newShaper(bytesPerSec int) *shaper {
	r := float64(bytesPerSec)
	return &shaper{rate: r, burst: r, tokens: r, last: time.Now()}
}

// take blocks until n bytes of budget are available, pacing the caller to the rate.
func (s *shaper) take(n int) {
	for {
		s.mu.Lock()
		now := time.Now()
		s.tokens += s.rate * now.Sub(s.last).Seconds()
		if s.tokens > s.burst {
			s.tokens = s.burst
		}
		s.last = now
		if s.tokens >= float64(n) {
			s.tokens -= float64(n)
			s.mu.Unlock()
			return
		}
		wait := time.Duration((float64(n) - s.tokens) / s.rate * float64(time.Second))
		s.mu.Unlock()
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		time.Sleep(wait)
	}
}

// ParseBitrate parses a human bitrate ("2mbit", "256kbit", "500kbps") or a bare
// number (bits per second) into bytes per second. "" returns 0 (unlimited).
func ParseBitrate(s string) (int, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	mult := 1.0 // -> bits/sec
	switch {
	case strings.HasSuffix(s, "mbit") || strings.HasSuffix(s, "mbps"):
		mult = 1e6
		s = strings.TrimSuffix(strings.TrimSuffix(s, "mbit"), "mbps")
	case strings.HasSuffix(s, "kbit") || strings.HasSuffix(s, "kbps"):
		mult = 1e3
		s = strings.TrimSuffix(strings.TrimSuffix(s, "kbit"), "kbps")
	case strings.HasSuffix(s, "bit") || strings.HasSuffix(s, "bps"):
		s = strings.TrimSuffix(strings.TrimSuffix(s, "bit"), "bps")
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("mux: bad bitrate %q", s)
	}
	return int(v * mult / 8), nil // bytes/sec
}
