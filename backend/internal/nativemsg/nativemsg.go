// Package nativemsg implements Chrome's Native Messaging wire protocol.
//
// The protocol is deliberately tiny:
//
//	+--------------------------+-------------------------------+
//	| 4 bytes, uint32 LITTLE   | UTF-8 JSON payload, N bytes   |
//	| ENDIAN, = N (byte count) |                               |
//	+--------------------------+-------------------------------+
//
// Rules that are easy to get wrong and that break the pipe silently:
//
//   - The length header is ALWAYS little endian, on every platform, regardless
//     of the machine's native byte order. (encoding/binary.LittleEndian.)
//   - stdout is the message channel. Printing a single stray byte to stdout
//     (a fmt.Println, a panic trace, a library log) desynchronises the stream
//     and Chrome kills the host. Send all logs to stderr or a file.
//   - Chrome accepts at most 1 MiB per message from the host; the host may
//     receive up to 4 GiB from Chrome. We cap reads defensively.
//   - A clean EOF on stdin means "Chrome closed the port": exit with status 0.
package nativemsg

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	// MaxReadSize guards against a corrupt/hostile length header allocating
	// gigabytes. 64 MiB is far more than any control message we exchange.
	MaxReadSize = 64 << 20
	// MaxWriteSize is Chrome's hard limit for host -> browser messages.
	MaxWriteSize = 1 << 20
)

// ErrMessageTooLarge is returned when a payload exceeds the protocol limits.
var ErrMessageTooLarge = errors.New("nativemsg: message too large")

// Reader decodes framed messages from a stream (normally os.Stdin).
type Reader struct {
	br *bufio.Reader
}

// NewReader wraps r with a buffered reader.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64<<10)}
}

// ReadRaw reads exactly one message and returns its raw JSON bytes.
// It returns io.EOF when the browser closes the pipe - that is a normal
// shutdown, not an error condition.
func (r *Reader) ReadRaw() ([]byte, error) {
	var header [4]byte

	// io.ReadFull distinguishes "clean EOF before any byte" (io.EOF) from
	// "EOF in the middle of the header" (io.ErrUnexpectedEOF). Only the first
	// one is a graceful shutdown.
	if _, err := io.ReadFull(r.br, header[:]); err != nil {
		return nil, err
	}

	length := binary.LittleEndian.Uint32(header[:])
	if length == 0 {
		return []byte("null"), nil
	}
	if length > MaxReadSize {
		return nil, fmt.Errorf("%w: %d bytes announced", ErrMessageTooLarge, length)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r.br, payload); err != nil {
		// A truncated payload means the stream is unusable from here on.
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return payload, nil
}

// Read decodes one message into v (a pointer).
func (r *Reader) Read(v any) error {
	raw, err := r.ReadRaw()
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// Writer encodes framed messages onto a stream (normally os.Stdout).
// It is safe for concurrent use: a mutex guarantees that a header is never
// separated from its payload by another goroutine's write.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter wraps w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write marshals v to JSON and emits <4-byte LE length><payload> atomically.
func (w *Writer) Write(v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("nativemsg: marshal: %w", err)
	}
	if len(payload) > MaxWriteSize {
		return fmt.Errorf("%w: %d bytes", ErrMessageTooLarge, len(payload))
	}

	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], uint32(len(payload)))

	// Build one contiguous buffer so a single Write syscall carries both the
	// header and the body; partial writes cannot desynchronise the framing.
	frame := make([]byte, 0, 4+len(payload))
	frame = append(frame, header[:]...)
	frame = append(frame, payload...)

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.w.Write(frame); err != nil {
		return fmt.Errorf("nativemsg: write: %w", err)
	}
	if f, ok := w.w.(interface{ Sync() error }); ok {
		_ = f.Sync() // best effort flush for os.File
	}
	return nil
}
