package server

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"net"
	"testing"
	"time"
)

// TestHandshakeAcceptRFCVector checks the magic GUID against the worked
// example in RFC 6455 section 1.3. Getting one character wrong here produces a
// handshake every browser silently rejects, so this test is the guard rail.
func TestHandshakeAcceptRFCVector(t *testing.T) {
	const (
		key  = "dGhlIHNhbXBsZSBub25jZQ=="
		want = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	)
	sum := sha1.Sum([]byte(key + magicGUID))
	got := base64.StdEncoding.EncodeToString(sum[:])
	if got != want {
		t.Fatalf("Sec-WebSocket-Accept mismatch:\n got %q\nwant %q\n(magicGUID = %q)", got, want, magicGUID)
	}
}

func TestWriteFrameHeaderLengths(t *testing.T) {
	cases := []struct {
		name        string
		size        int
		wantLenByte byte
		wantHeader  int
	}{
		{"tiny", 5, 5, 2},
		{"medium", 300, 126, 4},
		{"large", 70000, 127, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			c := &wsConn{conn: nopConn{}, bw: bufio.NewWriter(&buf)}
			if err := c.writeFrame(opText, bytes.Repeat([]byte("a"), tc.size)); err != nil {
				t.Fatalf("writeFrame: %v", err)
			}
			out := buf.Bytes()
			if out[0] != 0x81 {
				t.Fatalf("first byte = %#x, want 0x81 (FIN + text)", out[0])
			}
			if out[1] != tc.wantLenByte {
				t.Fatalf("length byte = %d, want %d", out[1], tc.wantLenByte)
			}
			if out[1]&0x80 != 0 {
				t.Fatal("server frames must not set the MASK bit")
			}
			if len(out) != tc.wantHeader+tc.size {
				t.Fatalf("total length = %d, want %d", len(out), tc.wantHeader+tc.size)
			}
		})
	}
}

// TestReadFrameUnmasks feeds a masked client frame and checks we decode it.
func TestReadFrameUnmasks(t *testing.T) {
	payload := []byte("hello")
	mask := [4]byte{0x37, 0xfa, 0x21, 0x3d}

	var raw bytes.Buffer
	raw.WriteByte(0x81)                      // FIN + text
	raw.WriteByte(byte(0x80 | len(payload))) // MASK + length
	raw.Write(mask[:])
	for i, b := range payload {
		raw.WriteByte(b ^ mask[i%4])
	}

	c := &wsConn{br: bufio.NewReader(&raw)}
	op, got, err := c.readFrame()
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if op != opText {
		t.Fatalf("opcode = %#x, want text", op)
	}
	if string(got) != "hello" {
		t.Fatalf("payload = %q, want %q", got, "hello")
	}
}

func TestOriginAllowed(t *testing.T) {
	allowed := []string{"", "chrome-extension://abcdefghijklmnop", "http://127.0.0.1:9090", "http://localhost:9090"}
	for _, o := range allowed {
		if !originAllowed(o, 9090) {
			t.Errorf("origin %q should be allowed", o)
		}
	}
	denied := []string{"https://evil.example", "http://127.0.0.1:9999", "http://attacker.localhost"}
	for _, o := range denied {
		if originAllowed(o, 9090) {
			t.Errorf("origin %q should be denied", o)
		}
	}
}

// nopConn is a net.Conn stub: writeFrame only needs SetWriteDeadline.
type nopConn struct{ net.Conn }

func (nopConn) SetWriteDeadline(time.Time) error { return nil }
func (nopConn) Close() error                     { return nil }
