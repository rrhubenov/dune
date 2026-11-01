package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
)

// PTY sessions: the host sends "GET /pty?cols=C&rows=R" with
// "Upgrade: dune-pty"; after "101 Switching Protocols" both sides exchange
// frames: 1 byte type, 4 bytes big-endian payload length, payload.
const (
	FrameData   byte = 0 // raw terminal bytes, either direction
	FrameResize byte = 1 // host->guest: cols uint16, rows uint16
	FrameExit   byte = 2 // guest->host: exit code int32; last frame
)

const (
	PTYUpgrade      = "dune-pty"
	maxFramePayload = 1 << 20
)

func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	hdr := make([]byte, 5, 5+len(payload))
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	_, err := w.Write(append(hdr, payload...))
	return err
}

func ReadFrame(r io.Reader) (typ byte, payload []byte, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFramePayload {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds limit", n)
	}
	payload = make([]byte, n)
	_, err = io.ReadFull(r, payload)
	return hdr[0], payload, err
}

func ResizePayload(cols, rows uint16) []byte {
	b := make([]byte, 4)
	binary.BigEndian.PutUint16(b, cols)
	binary.BigEndian.PutUint16(b[2:], rows)
	return b
}

// PTY is the host side of a guest terminal session.
type PTY struct {
	conn net.Conn
	r    *bufio.Reader
	wmu  sync.Mutex
}

// Write sends terminal input.
func (p *PTY) Write(b []byte) (int, error) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if err := WriteFrame(p.conn, FrameData, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (p *PTY) Resize(cols, rows uint16) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	return WriteFrame(p.conn, FrameResize, ResizePayload(cols, rows))
}

// Next returns the next chunk of terminal output, or exited=true with the
// shell's exit code once it has ended.
func (p *PTY) Next() (data []byte, exited bool, code int, err error) {
	for {
		typ, payload, err := ReadFrame(p.r)
		if err != nil {
			return nil, false, 0, err
		}
		switch typ {
		case FrameData:
			return payload, false, 0, nil
		case FrameExit:
			if len(payload) == 4 {
				code = int(int32(binary.BigEndian.Uint32(payload)))
			}
			return nil, true, code, nil
		}
	}
}

func (p *PTY) Close() error { return p.conn.Close() }

// OpenPTY starts a login shell in the guest on a terminal of the given size.
func (c *Client) OpenPTY(ctx context.Context, cols, rows uint16) (*PTY, error) {
	conn, err := c.upgrade(ctx, fmt.Sprintf("/pty?cols=%d&rows=%d", cols, rows), PTYUpgrade)
	if err != nil {
		return nil, err
	}
	return &PTY{conn: conn, r: bufio.NewReader(conn)}, nil
}
