package snapshot

import (
	"strings"
	"testing"
)

func TestResticID(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"1a2b3c4d", true},                      // the short form restic itself prints
		{"deadbeef12345678", true},              // and any length up to the full id
		{strings.Repeat("ab", 32), true},        // 64: a whole sha256
		{"1a2b3c4", false},                      // 7 is too short
		{strings.Repeat("ab", 32) + "c", false}, // 65 is too long
		{"latest", false},                       // "latest" is a ref, not an id
		{"1A2B3C4D", false},                     // restic ids are lowercase hex
		{"DEADBEEF", false},
		{"1a2b3c4g", false}, // not hex
		{"", false},
		{"../../etc/passwd", false},     // no path junk reaches restic either
		{"2026-11-22T10-05-00Z", false}, // the old snapshot-name shape must not pass
	}
	for _, tt := range tests {
		t.Run(tt.s, func(t *testing.T) {
			if got := ResticID(tt.s); got != tt.want {
				t.Errorf("ResticID(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

func TestRetentionIsZero(t *testing.T) {
	tests := []struct {
		name string
		r    Retention
		want bool
	}{
		{"unset", Retention{}, true},
		{"hourly", Retention{Hourly: 1}, false},
		{"daily", Retention{Daily: 1}, false},
		{"weekly", Retention{Weekly: 1}, false},
		{"monthly", Retention{Monthly: 1}, false},
		{"full", Retention{Hourly: 24, Daily: 30, Weekly: 12, Monthly: 24}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.IsZero(); got != tt.want {
				t.Errorf("%+v IsZero = %v, want %v", tt.r, got, tt.want)
			}
		})
	}
}

// Default is the fallback callers get for a zero Retention, so it must never itself be zero.
func TestDefaultIsNotEmpty(t *testing.T) {
	if Default.IsZero() {
		t.Fatal("Default must not be the zero value; a zero fallback would keep nothing")
	}
	if Default.Hourly == 0 || Default.Daily == 0 || Default.Weekly == 0 || Default.Monthly == 0 {
		t.Errorf("Default = %+v, want a count in every bucket", Default)
	}
}
