package mqtt

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

func TestTopicMatch(t *testing.T) {
	for _, c := range []struct {
		f, t string
		ok   bool
	}{
		{"a/b", "a/b", true}, {"a/+", "a/b", true}, {"a/+/c", "a/b/c", true}, {"a/#", "a/b/c", true},
		{"a/+", "a/b/c", false}, {"a/b", "a/c", false}, {"#", "x/y", true}, {"a/+/c", "a/b/d", false},
	} {
		if TopicMatch(c.f, c.t) != c.ok {
			t.Errorf("TopicMatch(%q, %q) != %v", c.f, c.t, c.ok)
		}
	}
}

func TestPacketLength(t *testing.T) {
	for _, n := range []int{0, 127, 128, 16383, 16384, 300000} {
		p := packet(0x30, make([]byte, n))
		typ, body, err := readPacket(bufio.NewReader(bytes.NewReader(p)))
		if err != nil || typ != 0x30 || len(body) != n {
			t.Fatalf("len %d: %v %d", n, err, len(body))
		}
	}
}

// fakeBroker accepts one client, checks CONNECT, echoes PUBLISHes to the
// client when they match its subscription, and replies to PINGREQ.
func fakeBroker(t *testing.T, refuse byte) (string, chan []byte) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	connect := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		_, body, err := readPacket(br)
		if err != nil {
			return
		}
		connect <- body
		c.Write([]byte{0x20, 2, 0, refuse})
		if refuse != 0 {
			return
		}
		var sub string
		for {
			typ, body, err := readPacket(br)
			if err != nil {
				return
			}
			switch typ >> 4 {
			case 8: // SUBSCRIBE
				n := int(binary.BigEndian.Uint16(body[2:]))
				sub = string(body[4 : 4+n])
				c.Write([]byte{0x90, 3, body[0], body[1], 0})
			case 3:
				n := int(binary.BigEndian.Uint16(body))
				if TopicMatch(sub, string(body[2:2+n])) {
					c.Write(packet(typ, body))
				}
			case 12:
				c.Write([]byte{0xD0, 0})
			}
		}
	}()
	return ln.Addr().String(), connect
}

func TestClient(t *testing.T) {
	addr, connect := fakeBroker(t, 0)
	got := make(chan Message, 4)
	c, err := Dial(context.Background(), Options{Addr: addr, ClientID: "ytg-test", Username: "u", Password: "p",
		Will: &Message{Topic: "ytguard/pc/status", Payload: []byte("offline"), Retain: true}}, func(m Message) { got <- m })
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cb := <-connect
	for _, want := range [][]byte{[]byte("MQTT"), []byte("ytg-test"), []byte("ytguard/pc/status"), []byte("offline"), []byte("u"), []byte("p")} {
		if !bytes.Contains(cb, want) {
			t.Errorf("CONNECT lacks %q", want)
		}
	}
	if flags := cb[7]; flags != 0x02|0x04|0x20|0x80|0x40 {
		t.Errorf("connect flags %08b", flags)
	}
	if err := c.Subscribe("ytguard/pc/kid/+/cmd"); err != nil {
		t.Fatal(err)
	}
	c.Publish("ytguard/pc/kid/1/cmd", []byte("bonus:15"), false)
	c.Publish("ytguard/other", []byte("x"), true)
	select {
	case m := <-got:
		if m.Topic != "ytguard/pc/kid/1/cmd" || string(m.Payload) != "bonus:15" {
			t.Fatalf("%+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no message")
	}
}

func TestRefused(t *testing.T) {
	addr, _ := fakeBroker(t, 4)
	_, err := Dial(context.Background(), Options{Addr: addr, ClientID: "x"}, nil)
	if err == nil || err.Error() != "MQTT broker refused the connection: wrong username or password" {
		t.Fatalf("err = %v", err)
	}
}
