// Package output keeps workloads' output (ADR-0024): it stores what agents ship, with credentials
// removed, and removes output after the retention period or over a workload's size limit, keeping
// count of everything removed or never received so readers are told (ADR-0015: not collected, or
// no longer kept, is said, never silent).
package output

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/krishnanand20/einsora/controlplane/internal/domain"
	"github.com/krishnanand20/einsora/controlplane/internal/redact"
	"github.com/krishnanand20/einsora/controlplane/internal/store"
)

// Piece is what an agent sent for one of a workload's streams: Data from Offset on, in the stream
// the driver identifies as Source. Restart says the stream started over at 0 under the same Source.
type Piece struct {
	Stream  domain.OutputStream
	Source  string
	Restart bool
	Offset  int64
	Data    []byte
}

// Append stores p for workload w and returns the offset the stream is stored up to, where its agent
// sends from next. Output already stored is skipped; a gap is recorded as lost, with a note in the
// output where it happened. Then the workload's oldest output is removed if it's over maxBytes.
func Append(ctx context.Context, q store.Queries, w domain.Workload, p Piece, maxBytes int64, now time.Time) (int64, error) {
	st, err := q.OutputState(ctx, w.ID, p.Stream)
	switch {
	case domain.IsNotFound(err):
		st = domain.OutputState{WorkloadID: w.ID, Stream: p.Stream, Source: p.Source}
	case err != nil:
		return 0, err
	}
	if p.Source != st.Source || p.Restart {
		if st.Source != "" { // not its first output: say where the stream started over
			note := fmt.Sprintf("%s from here on comes from a new file on its machine: a new run, or another machine\n", p.Stream)
			if p.Source == st.Source {
				note = fmt.Sprintf("%s from here on starts over: its file on its machine was emptied\n", p.Stream)
			}
			if err := add(ctx, q, w.ID, domain.StreamEinsora, note, 0, now); err != nil {
				return 0, err
			}
		}
		st.Source, st.Next = p.Source, 0
	}
	data := p.Data
	switch {
	case p.Offset > st.Next:
		gap := p.Offset - st.Next
		st.LostBytes += gap
		st.Next = p.Offset
		note := fmt.Sprintf("%s of %s was lost here: it was gone from the machine before Einsora received it\n", Size(gap), p.Stream)
		if err := add(ctx, q, w.ID, domain.StreamEinsora, note, 0, now); err != nil {
			return 0, err
		}
	case p.Offset < st.Next:
		if skip := st.Next - p.Offset; skip >= int64(len(data)) {
			data = nil
		} else {
			data = data[skip:]
		}
	}
	if len(data) > 0 {
		text, redactions := Clean(data)
		if _, err := q.AppendOutput(ctx, domain.OutputChunk{WorkloadID: w.ID, Stream: p.Stream, At: now, Text: text, Redactions: redactions}); err != nil {
			return 0, err
		}
		st.Next += int64(len(data))
		st.StoredBytes += int64(len(text))
	}
	if err := q.PutOutputState(ctx, st); err != nil {
		return 0, err
	}
	return st.Next, trim(ctx, q, w.ID, maxBytes)
}

// add stores a chunk that isn't from the agent's stream offsets (a note), counting its bytes.
func add(ctx context.Context, q store.Queries, workloadID uuid.UUID, stream domain.OutputStream, text string, redactions int, now time.Time) error {
	if _, err := q.AppendOutput(ctx, domain.OutputChunk{WorkloadID: workloadID, Stream: stream, At: now, Text: text, Redactions: redactions}); err != nil {
		return err
	}
	st, err := state(ctx, q, workloadID, stream)
	if err != nil {
		return err
	}
	st.StoredBytes += int64(len(text))
	return q.PutOutputState(ctx, st)
}

// state returns a stream's state, or a new one before its first output.
func state(ctx context.Context, q store.Queries, workloadID uuid.UUID, stream domain.OutputStream) (domain.OutputState, error) {
	st, err := q.OutputState(ctx, workloadID, stream)
	if domain.IsNotFound(err) {
		return domain.OutputState{WorkloadID: workloadID, Stream: stream}, nil
	}
	return st, err
}

var streams = []domain.OutputStream{domain.StreamStdout, domain.StreamStderr, domain.StreamEinsora}

// trimRounds bounds how much one append trims, so its transaction stays short when there's a lot to
// remove (after the limit is lowered): the next append trims on.
const trimRounds = 8

// trim removes a workload's oldest output while it's over maxBytes, down to nine tenths of it, so
// it doesn't trim on every append.
func trim(ctx context.Context, q store.Queries, workloadID uuid.UUID, maxBytes int64) error {
	states := map[domain.OutputStream]domain.OutputState{}
	var total int64
	for _, s := range streams {
		st, err := state(ctx, q, workloadID, s)
		if err != nil {
			return err
		}
		states[s] = st
		total += st.StoredBytes
	}
	if total <= maxBytes {
		return nil
	}
	target := maxBytes / 10 * 9
	for round := 0; total > target && round < trimRounds; round++ {
		chunks, err := q.OldestOutput(ctx, workloadID, 256)
		if err != nil || len(chunks) == 0 {
			break
		}
		var ids []int64
		for _, c := range chunks {
			if total <= target {
				break
			}
			n := int64(len(c.Text))
			ids = append(ids, c.ID)
			total -= n
			st := states[c.Stream]
			st.StoredBytes, st.TrimmedBytes = max(0, st.StoredBytes-n), st.TrimmedBytes+n
			states[c.Stream] = st
		}
		if err := q.DeleteOutput(ctx, ids); err != nil {
			return err
		}
	}
	for _, s := range streams {
		if st := states[s]; st.TrimmedBytes > 0 || st.StoredBytes > 0 {
			if err := q.PutOutputState(ctx, st); err != nil {
				return err
			}
		}
	}
	return nil
}

// expireBatch is how many chunks Expire removes per transaction, so SQLite's one transaction at a
// time is never held for long.
const expireBatch = 500

// Expire removes the output of region's workloads received before before, counting it per stream
// as expired, and returns how many bytes it removed.
func Expire(ctx context.Context, st store.Store, region domain.Region, before time.Time) (int64, error) {
	var removed int64
	for {
		var n int
		err := st.InTx(ctx, func(q store.Queries) error {
			chunks, err := q.ExpiredOutput(ctx, region, before, expireBatch)
			if err != nil {
				return err
			}
			n = len(chunks)
			type key struct {
				workload uuid.UUID
				stream   domain.OutputStream
			}
			bytes := map[key]int64{}
			ids := make([]int64, len(chunks))
			for i, c := range chunks {
				ids[i] = c.ID
				bytes[key{c.WorkloadID, c.Stream}] += int64(len(c.Text))
			}
			for k, b := range bytes {
				s, err := state(ctx, q, k.workload, k.stream)
				if err != nil {
					return err
				}
				s.StoredBytes, s.ExpiredBytes = max(0, s.StoredBytes-b), s.ExpiredBytes+b
				if err := q.PutOutputState(ctx, s); err != nil {
					return err
				}
				removed += b
			}
			return q.DeleteOutput(ctx, ids)
		})
		if err != nil || n < expireBatch {
			return removed, err
		}
	}
}

// Clean makes output storable and safe to show its owner: valid UTF-8 without NUL characters
// (which Postgres can't store), and with credentials in URIs removed, such as a notebook server's
// ?token=. It returns how many credentials it removed. Everything else is kept as it was.
func Clean(b []byte) (string, int) {
	text := strings.ToValidUTF8(string(b), "�")
	text = strings.ReplaceAll(text, "\x00", "�")
	return redact.CountSecrets(text)
}

// Size describes a number of bytes for people: "512 bytes", "1.5 KiB", "64.0 MiB".
func Size(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d bytes", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
}
