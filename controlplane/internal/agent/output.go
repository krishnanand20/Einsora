package agent

import (
	"bytes"
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/agent/protocol"
	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/redact"
)

// OutputReader is a driver that keeps its workloads' output (ADR-0024): stdout and stderr, each in
// a file that only grows, until the workload is removed.
type OutputReader interface {
	// ReadOutput returns up to max bytes of a workload's stream from offset, the stream's source
	// (an identity that changes when its file is replaced), and the file's size. A workload or
	// stream the driver doesn't have is an error (fs.ErrNotExist).
	ReadOutput(ctx context.Context, id uuid.UUID, stream domain.OutputStream, offset int64, max int) (data []byte, source string, size int64, err error)
}

const (
	// outputEvery is how often the agent sends new output.
	outputEvery = time.Second
	// outputPartialAfter is how long output without a newline waits for the rest of its line, so
	// a line, and anything in it that must be redacted, usually arrives whole.
	outputPartialAfter = 2 * time.Second
	// flushRounds bounds sending a workload's last output before it's removed.
	flushRounds = 16
)

type outputKey struct {
	id     uuid.UUID
	stream domain.OutputStream
}

// outputCursor is how far the control plane has one of a workload's streams.
type outputCursor struct {
	known, ignored bool
	source         string
	next           int64
	partialSince   time.Time // when a partial line was first held back
}

// outputSource is a workload whose output a driver keeps.
type outputSource struct {
	id uuid.UUID
	r  OutputReader
}

var outputStreams = []domain.OutputStream{domain.StreamStdout, domain.StreamStderr}

// shipLoop sends the drivers' new output every outputEvery.
func (a *Agent) shipLoop(ctx context.Context) {
	ticker := time.NewTicker(outputEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		statuses, err := a.statuses(ctx)
		if err != nil {
			continue
		}
		var from []outputSource
		for id, h := range statuses {
			if r, ok := h.driver.(OutputReader); ok {
				from = append(from, outputSource{id, r})
			}
		}
		_ = a.shipOutput(ctx, from, false, true) // what's left goes next time
	}
}

// shipOutput sends what from's workloads have written that the control plane hasn't stored: whole
// lines, a partial line once it has waited outputPartialAfter, or everything with flush. What
// doesn't fit in one request goes next time; with flush, in up to flushRounds requests. prune
// forgets workloads not in from. It reports whether nothing is left to send; output that can't be
// sent now is sent later.
func (a *Agent) shipOutput(ctx context.Context, from []outputSource, flush, prune bool) bool {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	slices.SortFunc(from, func(x, y outputSource) int { return cmp.Compare(x.id.String(), y.id.String()) })
	if prune {
		for k := range a.out {
			if !slices.ContainsFunc(from, func(s outputSource) bool { return s.id == k.id }) {
				delete(a.out, k)
			}
		}
	}
	for range flushRounds {
		req := a.outputRequest(ctx, from, flush)
		if len(req.Chunks) == 0 {
			return true
		}
		resp, err := a.conn.Output(ctx, req)
		if err != nil {
			if !a.outFailing {
				a.log.Warn("couldn't send workloads' output; retrying", "err", redact.Secrets(err.Error()))
			}
			a.outFailing = true
			return false
		}
		a.outFailing = false
		for _, c := range resp.Streams {
			if cur := a.out[outputKey{c.ID, c.Stream}]; cur != nil {
				cur.known, cur.ignored, cur.source, cur.next = true, c.Ignored, c.Source, c.Next
			}
		}
		if !flush {
			return false
		}
	}
	return false
}

// outputRequest gathers what to send. Call with a.outMu held.
func (a *Agent) outputRequest(ctx context.Context, from []outputSource, flush bool) protocol.OutputRequest {
	req := protocol.OutputRequest{Version: protocol.Version}
	size := 0
	now := a.clock()
	for _, s := range from {
		for _, stream := range outputStreams {
			if len(req.Chunks) == protocol.MaxOutputChunks || size >= protocol.MaxOutputRequest {
				return req
			}
			k := outputKey{s.id, stream}
			cur := a.out[k]
			if cur == nil {
				cur = &outputCursor{}
				a.out[k] = cur
			}
			if cur.ignored {
				continue
			}
			if !cur.known {
				// Where does the control plane have it up to? It answers with the offset to send from.
				if _, source, _, err := s.r.ReadOutput(ctx, s.id, stream, 0, 0); err == nil {
					req.Chunks = append(req.Chunks, protocol.OutputChunk{ID: s.id, Stream: stream, Source: source})
				}
				continue
			}
			limit := min(protocol.MaxOutputChunk, protocol.MaxOutputRequest-size)
			data, source, fileSize, err := s.r.ReadOutput(ctx, s.id, stream, cur.next, limit)
			if err != nil {
				continue
			}
			offset, restart := cur.next, false
			if source != cur.source || fileSize < cur.next {
				// A new file, or this one started over: send it from the start.
				restart, offset = source == cur.source, 0
				if data, source, _, err = s.r.ReadOutput(ctx, s.id, stream, 0, limit); err != nil {
					continue
				}
			}
			data = sendable(cur, data, limit, flush, now)
			if len(data) == 0 && offset == cur.next && source == cur.source {
				continue
			}
			req.Chunks = append(req.Chunks, protocol.OutputChunk{ID: s.id, Stream: stream, Source: source,
				Restart: restart, Offset: offset, Data: data})
			size += len(data)
		}
	}
	return req
}

// sendable is what of data, read up to limit bytes, to send now. Output is cut only between lines,
// so redaction sees each line, and any credential in it, whole: a read that filled limit sends up
// to its last newline, and the rest goes next time. Only a line longer than a whole chunk goes in
// pieces, since it can't wait for its end. Otherwise, a partial line waits outputPartialAfter for
// the rest, or not at all with flush.
func sendable(cur *outputCursor, data []byte, limit int, flush bool, now time.Time) []byte {
	if len(data) < limit || limit <= 0 {
		return wholeLines(cur, data, flush, now)
	}
	cur.partialSince = time.Time{}
	if end := bytes.LastIndexByte(data, '\n') + 1; end > 0 {
		return data[:end]
	}
	if limit < protocol.MaxOutputChunk {
		return nil // no room for this line in this request: the next has
	}
	return data
}

// wholeLines is what of data to send now: its whole lines, keeping a partial line back until it
// has waited outputPartialAfter, or all of it.
func wholeLines(cur *outputCursor, data []byte, all bool, now time.Time) []byte {
	end := bytes.LastIndexByte(data, '\n') + 1
	if all || end == len(data) {
		cur.partialSince = time.Time{}
		return data
	}
	if cur.partialSince.IsZero() {
		cur.partialSince = now
	}
	if now.Sub(cur.partialSince) >= outputPartialAfter {
		cur.partialSince = time.Time{}
		return data
	}
	return data[:end]
}
