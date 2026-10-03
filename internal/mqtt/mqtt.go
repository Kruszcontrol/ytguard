// Package mqtt is a small MQTT 3.1.1 client: just what YTGuard needs to
// talk to Home Assistant's broker (QoS 0 publish/subscribe, retained
// messages, a last-will message, keep-alive and TLS).
package mqtt

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Message is a received or published message.
type Message struct {
	Topic   string
	Payload []byte
	Retain  bool
}

// Options configure a connection.
type Options struct {
	Addr      string // host:port
	TLS       *tls.Config
	ClientID  string
	Username  string
	Password  string
	KeepAlive time.Duration // default 30s
	Will      *Message      // published by the broker if we disappear
}

// Client is a connected MQTT client. It's safe for concurrent use.
type Client struct {
	conn      net.Conn
	wmu       sync.Mutex
	onMessage func(Message)
	done      chan struct{}
	err       error
	errOnce   sync.Once
	nextID    uint16
	lastRecv  int64 // unix nanos, accessed under wmu
	keepAlive time.Duration
}

const maxPacket = 1 << 20

var connackErrors = map[byte]string{
	1: "unsupported protocol version", 2: "client ID rejected", 3: "broker unavailable",
	4: "wrong username or password", 5: "not authorized",
}

// Dial connects and logs in. onMessage is called (from one goroutine) for
// every message on subscribed topics.
func Dial(ctx context.Context, o Options, onMessage func(Message)) (*Client, error) {
	if o.KeepAlive == 0 {
		o.KeepAlive = 30 * time.Second
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if o.TLS != nil {
		conn, err = (&tls.Dialer{NetDialer: d, Config: o.TLS}).DialContext(ctx, "tcp", o.Addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", o.Addr)
	}
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, onMessage: onMessage, done: make(chan struct{}), keepAlive: o.KeepAlive}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(connectPacket(o)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	typ, body, err := readPacket(br)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("MQTT handshake: %w", err)
	}
	if typ>>4 != 2 || len(body) < 2 {
		conn.Close()
		return nil, errors.New("MQTT handshake: unexpected reply (is this an MQTT broker?)")
	}
	if body[1] != 0 {
		conn.Close()
		if msg, ok := connackErrors[body[1]]; ok {
			return nil, errors.New("MQTT broker refused the connection: " + msg)
		}
		return nil, fmt.Errorf("MQTT broker refused the connection (code %d)", body[1])
	}
	conn.SetDeadline(time.Time{})
	c.touch()
	go c.readLoop(br)
	go c.pingLoop()
	return c, nil
}

// Done is closed when the connection ends.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the connection ended.
func (c *Client) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

func (c *Client) fail(err error) {
	c.errOnce.Do(func() {
		c.err = err
		c.conn.Close()
		close(c.done)
	})
}

func (c *Client) touch() {
	c.wmu.Lock()
	c.lastRecv = time.Now().UnixNano()
	c.wmu.Unlock()
}

func (c *Client) write(p []byte) error {
	select {
	case <-c.done:
		return c.err
	default:
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := c.conn.Write(p)
	if err != nil {
		go c.fail(err)
	}
	return err
}

// Publish sends a QoS 0 message.
func (c *Client) Publish(topic string, payload []byte, retain bool) error {
	if topic == "" {
		return errors.New("empty topic")
	}
	return c.write(publishPacket(topic, payload, retain))
}

// Subscribe asks for messages matching the filters (QoS 0).
func (c *Client) Subscribe(filters ...string) error {
	c.wmu.Lock()
	c.nextID++
	if c.nextID == 0 {
		c.nextID = 1
	}
	id := c.nextID
	c.wmu.Unlock()
	var body []byte
	body = binary.BigEndian.AppendUint16(body, id)
	for _, f := range filters {
		body = appendString(body, f)
		body = append(body, 0) // QoS 0
	}
	return c.write(packet(0x82, body))
}

// Close disconnects cleanly (the will message is not sent).
func (c *Client) Close() {
	_ = c.write([]byte{0xE0, 0})
	c.fail(errors.New("closed"))
}

func (c *Client) readLoop(br *bufio.Reader) {
	for {
		typ, body, err := readPacket(br)
		if err != nil {
			c.fail(err)
			return
		}
		c.touch()
		switch typ >> 4 {
		case 3: // PUBLISH
			qos := (typ >> 1) & 3
			if len(body) < 2 {
				c.fail(errors.New("bad PUBLISH"))
				return
			}
			n := int(binary.BigEndian.Uint16(body))
			if len(body) < 2+n {
				c.fail(errors.New("bad PUBLISH"))
				return
			}
			topic, rest := string(body[2:2+n]), body[2+n:]
			if qos > 0 {
				if len(rest) < 2 {
					c.fail(errors.New("bad PUBLISH"))
					return
				}
				id := rest[:2]
				rest = rest[2:]
				ack := byte(0x40) // PUBACK
				if qos == 2 {
					ack = 0x50 // PUBREC (we don't track QoS 2 further)
				}
				_ = c.write([]byte{ack, 2, id[0], id[1]})
			}
			if c.onMessage != nil {
				c.onMessage(Message{Topic: topic, Payload: append([]byte(nil), rest...), Retain: typ&1 == 1})
			}
		case 6: // PUBREL (QoS 2 from broker): complete it
			if len(body) >= 2 {
				_ = c.write([]byte{0x70, 2, body[0], body[1]})
			}
		}
	}
}

func (c *Client) pingLoop() {
	t := time.NewTicker(c.keepAlive / 2)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
		}
		c.wmu.Lock()
		last := time.Unix(0, c.lastRecv)
		c.wmu.Unlock()
		if time.Since(last) > c.keepAlive*3/2 {
			c.fail(errors.New("MQTT broker stopped responding"))
			return
		}
		if c.write([]byte{0xC0, 0}) != nil {
			return
		}
	}
}

// ---- encoding ----

func connectPacket(o Options) []byte {
	var flags byte = 0x02 // clean session
	if o.Will != nil {
		flags |= 0x04
		if o.Will.Retain {
			flags |= 0x20
		}
	}
	if o.Username != "" {
		flags |= 0x80
		if o.Password != "" {
			flags |= 0x40
		}
	}
	body := appendString(nil, "MQTT")
	body = append(body, 4, flags)
	body = binary.BigEndian.AppendUint16(body, uint16(o.KeepAlive/time.Second))
	body = appendString(body, o.ClientID)
	if o.Will != nil {
		body = appendString(body, o.Will.Topic)
		body = appendBytes(body, o.Will.Payload)
	}
	if o.Username != "" {
		body = appendString(body, o.Username)
		if o.Password != "" {
			body = appendString(body, o.Password)
		}
	}
	return packet(0x10, body)
}

func publishPacket(topic string, payload []byte, retain bool) []byte {
	var h byte = 0x30
	if retain {
		h |= 1
	}
	body := appendString(nil, topic)
	return packet(h, append(body, payload...))
}

func packet(header byte, body []byte) []byte {
	out := []byte{header}
	n := len(body)
	for {
		b := byte(n % 128)
		n /= 128
		if n > 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			break
		}
	}
	return append(out, body...)
}

func appendString(b []byte, s string) []byte { return appendBytes(b, []byte(s)) }

func appendBytes(b, s []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(s)))
	return append(b, s...)
}

func readPacket(r *bufio.Reader) (byte, []byte, error) {
	typ, err := r.ReadByte()
	if err != nil {
		return 0, nil, err
	}
	n, mult := 0, 1
	for i := 0; ; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, nil, err
		}
		n += int(b&0x7f) * mult
		if b&0x80 == 0 {
			break
		}
		if i == 3 {
			return 0, nil, errors.New("bad packet length")
		}
		mult *= 128
	}
	if n > maxPacket {
		return 0, nil, fmt.Errorf("packet too large (%d bytes)", n)
	}
	body := make([]byte, n)
	_, err = io.ReadFull(r, body)
	return typ, body, err
}

// TopicMatch reports whether topic matches an MQTT filter with + and #.
func TopicMatch(filter, topic string) bool {
	for {
		fi, ti := indexSlash(filter), indexSlash(topic)
		fseg, tseg := filter[:fi], topic[:ti]
		if fseg == "#" {
			return true
		}
		if fseg != "+" && fseg != tseg {
			return false
		}
		fdone, tdone := fi == len(filter), ti == len(topic)
		if fdone || tdone {
			return fdone && tdone
		}
		filter, topic = filter[fi+1:], topic[ti+1:]
	}
}

func indexSlash(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return i
		}
	}
	return len(s)
}
