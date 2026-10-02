package peercred

import (
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
)

const sample = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1EC6 00000000:0000 0A 00000000:00000000 00:00000000 00000000   998        0 1 1 0000000000000000 100 0 0 10 0
   1: 0100007F:D431 0100007F:1EC6 01 00000000:00000000 00:00000000 00000000  1001        0 2 1 0000000000000000 20 4 30 10 -1
   2: 0100007F:1EC6 0100007F:D431 01 00000000:00000000 00:00000000 00000000   998        0 3 1 0000000000000000 20 4 30 10 -1
`

func TestFindUID(t *testing.T) {
	client := netip.MustParseAddrPort("127.0.0.1:54321")
	server := netip.MustParseAddrPort("127.0.0.1:7878")
	uid, err := findUID(strings.NewReader(sample), client, server)
	if err != nil || uid != 1001 {
		t.Fatalf("uid=%d err=%v", uid, err)
	}
}

func TestRealSocket(t *testing.T) {
	if _, err := os.Stat(ProcNetTCP); err != nil {
		t.Skip("no /proc/net/tcp")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); done <- c }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sc := <-done
	defer sc.Close()
	name, err := User(sc.RemoteAddr().String(), sc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	me, _ := Username(os.Getuid())
	if name != me {
		t.Fatalf("got %q want %q", name, me)
	}
}
