// Package rcon implements the minimal Source RCON protocol
// (https://wiki.vg/RCON) used by every Minecraft dedicated server.
package rcon

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	typeAuth        = 3
	typeExecCommand = 2
	typeResponse    = 0

	maxBodyLen   = 4096
	headerLen    = 8  // id + type
	trailerLen   = 2  // two trailing NUL bytes
	minPacketLen = 10 // header + trailer, empty payload
)

// ErrAuthFailed is returned when the server rejects the RCON password.
var ErrAuthFailed = errors.New("rcon: authentication failed")

// ErrTooLong is returned for commands exceeding the protocol limit.
var ErrTooLong = errors.New("rcon: command too long")

// Client is a single-threaded (mutex guarded) RCON connection.
type Client struct {
	conn net.Conn
	mu   sync.Mutex
	seq  int32
}

// Dial connects and authenticates against addr with the given password.
func Dial(addr, password string) (*Client, error) {
	return DialTimeout(addr, password, 5*time.Second)
}

// DialTimeout is Dial with a configurable connect timeout.
func DialTimeout(addr, password string, timeout time.Duration) (*Client, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("rcon dial %s: %w", addr, err)
	}
	c := &Client{conn: conn}
	if err := c.conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := c.auth(password); err != nil {
		_ = c.Close()
		return nil, err
	}
	// From now on the connection may block indefinitely while a server starts.
	_ = c.conn.SetDeadline(time.Time{})
	return c, nil
}

// Close terminates the underlying connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func (c *Client) auth(password string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID()
	if err := c.send(id, typeAuth, password); err != nil {
		return err
	}
	for {
		pkt, err := c.readPacket()
		if err != nil {
			return err
		}
		// A rejected login is answered with id -1.
		if pkt.id == -1 {
			return ErrAuthFailed
		}
		if pkt.id == id {
			return nil
		}
		// Some servers send an extra empty packet first; keep reading.
	}
}

// Command executes a single console command and returns the concatenated
// response payload. Multi-packet responses are reassembled by sending a
// sentinel packet after the command and reading until it is echoed back.
func (c *Client) Command(cmd string) (string, error) {
	if len(cmd)+minPacketLen > maxBodyLen+headerLen+trailerLen {
		return "", ErrTooLong
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID()
	if err := c.send(id, typeExecCommand, cmd); err != nil {
		return "", err
	}
	sentinel := c.nextID()
	if err := c.send(sentinel, typeExecCommand, ""); err != nil {
		return "", err
	}

	var sb strings.Builder
	for {
		pkt, err := c.readPacket()
		if err != nil {
			return sb.String(), err
		}
		if pkt.id == sentinel {
			return sb.String(), nil
		}
		if pkt.id == id {
			sb.WriteString(pkt.payload)
		}
	}
}

type packet struct {
	id      int32
	pktType int32
	payload string
}

func (c *Client) nextID() int32 {
	id := c.seq
	c.seq++
	// Skip the -1 id which the protocol reserves for auth failure.
	if c.seq == -1 {
		c.seq = 0
	}
	return id
}

func (c *Client) send(id, pktType int32, payload string) error {
	body := make([]byte, headerLen+len(payload)+trailerLen)
	binary.LittleEndian.PutUint32(body[0:4], uint32(id))
	binary.LittleEndian.PutUint32(body[4:8], uint32(pktType))
	copy(body[8:], payload)
	// trailing NULs are already zero

	length := uint32(len(body))
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, length)
	if _, err := c.conn.Write(append(header, body...)); err != nil {
		return err
	}
	return nil
}

func (c *Client) readPacket() (packet, error) {
	var length uint32
	if err := binary.Read(c.conn, binary.LittleEndian, &length); err != nil {
		return packet{}, err
	}
	if length < minPacketLen || length > maxBodyLen+headerLen+trailerLen {
		return packet{}, fmt.Errorf("rcon: bogus packet length %d", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(c.conn, buf); err != nil {
		return packet{}, err
	}
	pkt := packet{
		id:      int32(binary.LittleEndian.Uint32(buf[0:4])),
		pktType: int32(binary.LittleEndian.Uint32(buf[4:8])),
	}
	if len(buf) >= headerLen+trailerLen {
		payload := buf[headerLen : len(buf)-trailerLen]
		pkt.payload = string(payload)
	}
	return pkt, nil
}
