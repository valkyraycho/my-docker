package stdcopy

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

type StreamType byte

const (
	Stdin  StreamType = 0
	Stdout StreamType = 1
	Stderr StreamType = 2
)

// HeaderSize is the fixed-size prefix before every frame's payload.
const HeaderSize = 8

// Muxer serializes writes from multiple stream-specific writers onto a
// single underlying io.Writer. Use Muxer when multiple goroutines write
// to different streams concurrently — the internal mutex ensures frame
// headers and payloads never interleave.
type Muxer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewMuxer wraps w. The underlying writer is touched only while the
// muxer's mutex is held.
func NewMuxer(w io.Writer) *Muxer {
	return &Muxer{w: w}
}

// Stream returns a per-stream writer that emits frames tagged with s.
// The returned writer shares the Muxer's lock with every other stream
// writer from the same Muxer.
func (m *Muxer) Stream(s StreamType) *FrameWriter {
	return &FrameWriter{m: m, stream: s}
}

// FrameWriter emits one frame per Write call. Not usable standalone;
// construct via Muxer.Stream.
type FrameWriter struct {
	m      *Muxer
	stream StreamType
}

func (fw *FrameWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	header := make([]byte, HeaderSize)

	header[0] = byte(fw.stream)
	binary.BigEndian.PutUint32(header[4:], uint32(len(p)))

	fw.m.mu.Lock()
	defer fw.m.mu.Unlock()

	if _, err := fw.m.w.Write(header); err != nil {
		return 0, fmt.Errorf("write header: %w", err)
	}
	numBytes, err := fw.m.w.Write(p)
	if err != nil {
		return numBytes, fmt.Errorf("write payload: %w", err)
	}
	return numBytes, nil
}

func Demux(src io.Reader, stdout, stderr io.Writer) error {
	var header [HeaderSize]byte
	for {
		if _, err := io.ReadFull(src, header[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read header: %w", err)
		}
		stream := StreamType(header[0])
		length := binary.BigEndian.Uint32(header[4:])
		var dst io.Writer
		switch stream {
		case Stdout:
			dst = stdout
		case Stderr:
			dst = stderr
		default:
			return fmt.Errorf("unknown stream type %d", stream)
		}
		_, err := io.CopyN(dst, src, int64(length))
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			return fmt.Errorf("copy payload: %w", err)
		}
	}
}
