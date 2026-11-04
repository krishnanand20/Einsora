package agent

import (
	"bytes"
	"testing"
	"time"

	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
)

// Output is cut only between lines, unless one line is longer than a whole chunk, so redaction
// sees each line whole.
func TestSendable(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	lines := bytes.Repeat([]byte("token=abc 0123456789\n"), 20) // 420 bytes, whole lines
	for _, tt := range []struct {
		name  string
		data  []byte
		limit int
		flush bool
		want  int // bytes sent
	}{
		{"whole lines, under the limit", lines, protocol.MaxOutputChunk, false, len(lines)},
		{"a read cut by the request's room ends at its last newline", lines[:100], 100, false, 84},
		{"a full chunk of lines ends at its last newline", append(bytes.Repeat([]byte("x"), protocol.MaxOutputChunk-10), "\nmore line"...), protocol.MaxOutputChunk, false, protocol.MaxOutputChunk - 9},
		{"no newline and no room: the next request", bytes.Repeat([]byte("x"), 100), 100, false, 0},
		{"one line longer than a chunk goes in pieces", bytes.Repeat([]byte("x"), protocol.MaxOutputChunk), protocol.MaxOutputChunk, false, protocol.MaxOutputChunk},
		{"a partial line waits", []byte("loading"), protocol.MaxOutputChunk, false, 0},
		{"with flush, a partial line goes", []byte("loading"), protocol.MaxOutputChunk, true, 7},
		{"with flush, a cut read still ends at a newline", lines[:100], 100, true, 84},
	} {
		got := sendable(&outputCursor{}, tt.data, tt.limit, tt.flush, now)
		if len(got) != tt.want {
			t.Errorf("%s: sent %d bytes, want %d", tt.name, len(got), tt.want)
		}
	}

	// A partial line goes once it has waited outputPartialAfter.
	cur := &outputCursor{}
	if got := sendable(cur, []byte("a\nloading"), protocol.MaxOutputChunk, false, now); string(got) != "a\n" {
		t.Fatalf("first = %q", got)
	}
	if got := sendable(cur, []byte("loading"), protocol.MaxOutputChunk, false, now.Add(outputPartialAfter)); string(got) != "loading" {
		t.Errorf("after waiting = %q", got)
	}
}
