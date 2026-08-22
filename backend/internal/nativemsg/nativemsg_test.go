package nativemsg

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"testing"
)

// frame builds a wire message the way Chrome does.
func frame(payload string) []byte {
	var b bytes.Buffer
	var h [4]byte
	binary.LittleEndian.PutUint32(h[:], uint32(len(payload)))
	b.Write(h[:])
	b.WriteString(payload)
	return b.Bytes()
}

func TestReadRoundTrip(t *testing.T) {
	stream := append(frame(`{"type":"download","url":"https://x/y.mp4"}`), frame(`{"type":"ping"}`)...)
	r := NewReader(bytes.NewReader(stream))

	var first struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if err := r.Read(&first); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if first.Type != "download" || first.URL != "https://x/y.mp4" {
		t.Fatalf("unexpected first message: %+v", first)
	}

	var second struct {
		Type string `json:"type"`
	}
	if err := r.Read(&second); err != nil {
		t.Fatalf("second read: %v", err)
	}
	if second.Type != "ping" {
		t.Fatalf("unexpected second message: %+v", second)
	}

	// A clean EOF must be reported as io.EOF so the host can exit with status 0.
	if _, err := r.ReadRaw(); err != io.EOF {
		t.Fatalf("expected io.EOF at end of stream, got %v", err)
	}
}

func TestWriteUsesLittleEndianHeader(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	if err := w.Write(map[string]any{"ok": true}); err != nil {
		t.Fatalf("write: %v", err)
	}

	out := buf.Bytes()
	if len(out) < 4 {
		t.Fatalf("output too short: %d bytes", len(out))
	}
	got := binary.LittleEndian.Uint32(out[:4])
	body := out[4:]
	if int(got) != len(body) {
		t.Fatalf("header says %d bytes, body is %d bytes", got, len(body))
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if decoded["ok"] != true {
		t.Fatalf("payload mismatch: %v", decoded)
	}

	// Guard the classic bug: a big-endian header would put the length in the
	// last byte instead of the first for small messages.
	if out[0] == 0 && out[3] != 0 {
		t.Fatal("header looks big endian")
	}
}

func TestTruncatedPayloadIsNotEOF(t *testing.T) {
	full := frame(`{"type":"ping"}`)
	r := NewReader(bytes.NewReader(full[:len(full)-3]))
	if _, err := r.ReadRaw(); err != io.ErrUnexpectedEOF {
		t.Fatalf("expected io.ErrUnexpectedEOF for a truncated payload, got %v", err)
	}
}

func TestOversizedHeaderRejected(t *testing.T) {
	var h [4]byte
	binary.LittleEndian.PutUint32(h[:], MaxReadSize+1)
	r := NewReader(bytes.NewReader(h[:]))
	if _, err := r.ReadRaw(); err == nil {
		t.Fatal("expected an error for an oversized length header")
	}
}
