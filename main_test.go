package main

import (
	"testing"
	"time"
)

func TestPercentiles(t *testing.T) {
	latencies := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		300 * time.Millisecond,
		400 * time.Millisecond,
		500 * time.Millisecond,
		600 * time.Millisecond,
		700 * time.Millisecond,
		800 * time.Millisecond,
		900 * time.Millisecond,
		1000 * time.Millisecond,
	}
	m := computeMetrics(latencies)
	if m.Min != 100*time.Millisecond {
		t.Errorf("Min: got %v, want 100ms", m.Min)
	}
	if m.Max != 1000*time.Millisecond {
		t.Errorf("Max: got %v, want 1000ms", m.Max)
	}
	if m.Avg != 550*time.Millisecond {
		t.Errorf("Avg: got %v, want 550ms", m.Avg)
	}
	if m.P50 != 500*time.Millisecond {
		t.Errorf("P50: got %v, want 500ms", m.P50)
	}
	if m.P95 != 1000*time.Millisecond {
		t.Errorf("P95: got %v, want 1000ms", m.P95)
	}
	if m.P99 != 1000*time.Millisecond {
		t.Errorf("P99: got %v, want 1000ms", m.P99)
	}
}

func TestPercentilesEmpty(t *testing.T) {
	m := computeMetrics(nil)
	if m.Min != 0 || m.Max != 0 || m.Avg != 0 {
		t.Errorf("empty input should yield zero metrics, got %+v", m)
	}
}

func TestClassifyError(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantErrTyp string
	}{
		{"200 success", 200, ""},
		{"201 success", 201, ""},
		{"404 client error", 404, "client_error"},
		{"500 server error", 500, "server_error"},
		{"503 server error", 503, "server_error"},
		{"100 unknown", 100, "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyError(nil, c.status)
			if got != c.wantErrTyp {
				t.Errorf("status=%d: got %q, want %q", c.status, got, c.wantErrTyp)
			}
		})
	}
}
