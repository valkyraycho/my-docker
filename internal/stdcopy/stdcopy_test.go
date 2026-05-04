package stdcopy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// TestMuxer_RoundTrip: write one stdout frame, demux it back out,
// verify the payload lands on stdout and nothing on stderr. The
// simplest possible round-trip — if this fails the basic framing
// is broken.
func TestMuxer_RoundTrip(t *testing.T) {
	var buf bytes.Buffer
	mux := NewMuxer(&buf)

	payload := []byte("hello from stdout\n")
	n, err := mux.Stream(Stdout).Write(payload)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(payload) {
		t.Errorf("Write returned %d, want %d", n, len(payload))
	}

	// Bytes on the wire: 8-byte header + payload.
	if got, want := buf.Len(), HeaderSize+len(payload); got != want {
		t.Errorf("bytes written: got %d, want %d", got, want)
	}

	var gotOut, gotErr bytes.Buffer
	if err := Demux(&buf, &gotOut, &gotErr); err != nil {
		t.Fatalf("Demux: %v", err)
	}

	if gotOut.String() != string(payload) {
		t.Errorf("stdout: got %q, want %q", gotOut.String(), payload)
	}
	if gotErr.Len() != 0 {
		t.Errorf("stderr: got %q, want empty", gotErr.String())
	}
}

// TestMuxer_StderrRouting: ensure the stream byte is encoded and
// decoded correctly by writing to stderr and asserting the payload
// lands in the right demux bucket.
func TestMuxer_StderrRouting(t *testing.T) {
	var buf bytes.Buffer
	mux := NewMuxer(&buf)

	payload := []byte("oops\n")
	if _, err := mux.Stream(Stderr).Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var gotOut, gotErr bytes.Buffer
	if err := Demux(&buf, &gotOut, &gotErr); err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if gotErr.String() != string(payload) {
		t.Errorf("stderr: got %q, want %q", gotErr.String(), payload)
	}
	if gotOut.Len() != 0 {
		t.Errorf("stdout: got %q, want empty", gotOut.String())
	}
}

// TestMuxer_Interleaved: two goroutines write alternating frames to
// stdout and stderr through the same Muxer. Must be run with -race.
// The invariant: every frame header must be followed by exactly its
// declared payload bytes — no byte-level interleaving between frames.
// If the mutex is missing or broken, Demux fails to decode because the
// decoder hits garbage where it expected a header.
func TestMuxer_Interleaved(t *testing.T) {
	var buf syncBuffer // concurrent-safe so the test's Write contention
	// actually exercises the Muxer's mutex, not bytes.Buffer's racy append
	mux := NewMuxer(&buf)

	const iters = 200
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := range iters {
			msg := fmt.Sprintf("out-%d\n", i)
			if _, err := mux.Stream(Stdout).Write([]byte(msg)); err != nil {
				t.Errorf("stdout Write: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := range iters {
			msg := fmt.Sprintf("err-%d\n", i)
			if _, err := mux.Stream(Stderr).Write([]byte(msg)); err != nil {
				t.Errorf("stderr Write: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	var gotOut, gotErr bytes.Buffer
	if err := Demux(&buf, &gotOut, &gotErr); err != nil {
		t.Fatalf("Demux: %v", err)
	}

	// We don't care about ordering between streams (that's scheduler-
	// dependent), only that all iters frames arrive on each stream
	// and none cross-contaminate.
	outLines := strings.Count(gotOut.String(), "\n")
	errLines := strings.Count(gotErr.String(), "\n")
	if outLines != iters {
		t.Errorf("stdout line count: got %d, want %d", outLines, iters)
	}
	if errLines != iters {
		t.Errorf("stderr line count: got %d, want %d", errLines, iters)
	}
	// No cross-contamination: each stream's bytes contain only its prefix.
	if strings.Contains(gotOut.String(), "err-") {
		t.Error("stdout contains stderr data — frame boundary broken")
	}
	if strings.Contains(gotErr.String(), "out-") {
		t.Error("stderr contains stdout data — frame boundary broken")
	}
}

// TestMuxer_EmptyWrite: zero-byte Write should emit nothing and return
// (0, nil). Tests the early-return guard. Without it we'd emit a
// pathological 8-byte header declaring a 0-byte payload, which is
// technically valid but wasteful.
func TestMuxer_EmptyWrite(t *testing.T) {
	var buf bytes.Buffer
	mux := NewMuxer(&buf)

	n, err := mux.Stream(Stdout).Write(nil)
	if err != nil {
		t.Fatalf("Write(nil): %v", err)
	}
	if n != 0 {
		t.Errorf("n: got %d, want 0", n)
	}
	if buf.Len() != 0 {
		t.Errorf("bytes written: got %d, want 0", buf.Len())
	}
}

// TestDemux_TruncatedHeader: src has fewer than 8 bytes. ReadFull
// returns io.ErrUnexpectedEOF; Demux surfaces that as a wrapped error
// (not a clean nil return).
func TestDemux_TruncatedHeader(t *testing.T) {
	src := bytes.NewReader([]byte{0x01, 0x00, 0x00}) // 3 bytes only
	err := Demux(src, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected error on truncated header, got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected ErrUnexpectedEOF, got %v", err)
	}
}

// TestDemux_TruncatedPayload: header says 10 bytes, src provides 3.
// io.CopyN detects the mismatch and returns ErrUnexpectedEOF, which
// Demux surfaces wrapped.
func TestDemux_TruncatedPayload(t *testing.T) {
	// Header: stream=stdout (1), length=10.
	buf := bytes.Buffer{}
	buf.Write([]byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0A})
	buf.Write([]byte("abc")) // only 3 of 10 promised bytes

	err := Demux(&buf, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected error on truncated payload, got nil")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("expected ErrUnexpectedEOF, got %v", err)
	}
}

// TestDemux_UnknownStream: a frame with stream byte 0xFF is neither
// stdin/stdout/stderr. Demux must reject it — without this check a
// corrupted stream could silently drop payloads.
func TestDemux_UnknownStream(t *testing.T) {
	buf := bytes.Buffer{}
	// Header: stream=0xFF, length=4.
	buf.Write([]byte{0xFF, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x04})
	buf.Write([]byte("abcd"))

	err := Demux(&buf, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected error on unknown stream, got nil")
	}
	if !strings.Contains(err.Error(), "unknown stream") {
		t.Errorf("error %q: expected 'unknown stream' in message", err.Error())
	}
}

// TestDemux_EmptyReader: zero-byte reader is a clean EOF at a frame
// boundary. Returns nil (no error) — this represents the normal
// "container has exited, no more logs" case.
func TestDemux_EmptyReader(t *testing.T) {
	err := Demux(bytes.NewReader(nil), io.Discard, io.Discard)
	if err != nil {
		t.Errorf("empty reader should be clean EOF, got %v", err)
	}
}

// syncBuffer is a concurrent-safe bytes.Buffer. Used in
// TestMuxer_Interleaved so bytes.Buffer's internal append doesn't race
// — the point of that test is exercising Muxer's locking, not fighting
// bytes.Buffer's own concurrency bugs.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Read(p)
}
