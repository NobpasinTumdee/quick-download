package server

// Minimal RFC 6455 WebSocket server.
//
// The whole point of this file is to keep the engine dependency-free: it
// implements exactly the subset of the protocol a progress feed needs.
//
//	server -> client : unfragmented text frames, never masked
//	client -> server : ignored (except close and ping, which are answered)
//
// What is deliberately NOT implemented: fragmentation (continuation frames),
// per-message compression, and extensions. If you ever need those, swap this
// file for github.com/gorilla/websocket - the Hub API above it stays the same.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// magicGUID is the constant every WebSocket server must concatenate with the
// client key before hashing (RFC 6455, section 1.3).
// It is verified by the RFC's own test vector in ws_test.go.
const magicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Opcodes we care about.
const (
	opContinuation byte = 0x0
	opText         byte = 0x1
	opBinary       byte = 0x2
	opClose        byte = 0x8
	opPing         byte = 0x9
	opPong         byte = 0xA
)

const maxClientFrame = 1 << 20 // 1 MiB: clients only send tiny control messages

// wsConn is a hijacked TCP connection speaking WebSocket frames.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
}

// upgrade performs the HTTP/1.1 -> WebSocket handshake by hand.
func upgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		return nil, errors.New("not a websocket upgrade request")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return nil, errors.New("unsupported websocket version")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("missing Sec-WebSocket-Key")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("connection does not support hijacking")
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return nil, fmt.Errorf("hijack: %w", err)
	}

	// accept = base64( SHA1( key + GUID ) )
	sum := sha1.Sum([]byte(key + magicGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])

	handshake := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"

	if _, err := buf.WriteString(handshake); err != nil {
		conn.Close()
		return nil, err
	}
	if err := buf.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{conn: conn, br: buf.Reader, bw: buf.Writer}, nil
}

// writeFrame emits one unfragmented, unmasked frame.
//
// Layout:
//
//	byte 0      : FIN(1) RSV(3) OPCODE(4)
//	byte 1      : MASK(1)=0 LEN(7)
//	bytes 2..   : extended length (16 or 64 bit, big endian) when LEN is 126/127
//	rest        : payload
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	header := make([]byte, 0, 10)
	header = append(header, 0x80|opcode) // FIN set, single frame

	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		header = append(header, 126)
		header = append(header, ext[:]...)
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(header, 127)
		header = append(header, ext[:]...)
	}

	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.bw.Write(header); err != nil {
		return err
	}
	if _, err := c.bw.Write(payload); err != nil {
		return err
	}
	return c.bw.Flush()
}

// readFrame reads one frame and unmasks it. Browsers always mask.
func (c *wsConn) readFrame() (opcode byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.br, head[:]); err != nil {
		return 0, nil, err
	}

	opcode = head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	if length > maxClientFrame {
		return 0, nil, fmt.Errorf("client frame too large: %d bytes", length)
	}

	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return 0, nil, err
		}
	}

	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

func (c *wsConn) close() error { return c.conn.Close() }
