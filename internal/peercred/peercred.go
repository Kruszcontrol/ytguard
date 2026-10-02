// Package peercred finds the Linux user that owns the client end of a
// loopback TCP connection, by looking it up in /proc/net/tcp.
//
// The extension talks to the daemon over 127.0.0.1 from the kid's own Chrome
// process, so the socket owner identifies which kid is asking.
package peercred

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
)

// ProcNetTCP is the table to read (overridable for tests).
var ProcNetTCP = "/proc/net/tcp"

// UID returns the uid owning the socket whose local address is client and
// remote address is server (i.e. the client side of a connection to us).
func UID(client, server netip.AddrPort) (int, error) {
	f, err := os.Open(ProcNetTCP)
	if err != nil {
		return -1, err
	}
	defer f.Close()
	return findUID(f, client, server)
}

func findUID(r io.Reader, client, server netip.AddrPort) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Scan() // header
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 8 {
			continue
		}
		local, err1 := parseAddr(f[1])
		remote, err2 := parseAddr(f[2])
		if err1 != nil || err2 != nil {
			continue
		}
		if local == client && remote == server {
			return strconv.Atoi(f[7])
		}
	}
	if err := sc.Err(); err != nil {
		return -1, err
	}
	return -1, fmt.Errorf("peercred: no socket %s -> %s", client, server)
}

// parseAddr parses "0100007F:1EC6" (IPv4, host byte order on little-endian).
func parseAddr(s string) (netip.AddrPort, error) {
	ipHex, portHex, ok := strings.Cut(s, ":")
	if !ok || len(ipHex) != 8 {
		return netip.AddrPort{}, fmt.Errorf("bad addr %q", s)
	}
	b, err := hex.DecodeString(ipHex)
	if err != nil {
		return netip.AddrPort{}, err
	}
	var ip [4]byte
	binary.BigEndian.PutUint32(ip[:], binary.LittleEndian.Uint32(b))
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}
	return netip.AddrPortFrom(netip.AddrFrom4(ip), uint16(port)), nil
}

var (
	nameMu    sync.Mutex
	nameCache = map[int]string{}
)

// Username returns the login name for uid.
func Username(uid int) (string, error) {
	nameMu.Lock()
	defer nameMu.Unlock()
	if n, ok := nameCache[uid]; ok {
		return n, nil
	}
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", err
	}
	nameCache[uid] = u.Username
	return u.Username, nil
}

// User resolves the login name of the client of a loopback connection.
// addrs are as given by http.Request.RemoteAddr and the listener address.
func User(remoteAddr, localAddr string) (string, error) {
	c, err := netip.ParseAddrPort(remoteAddr)
	if err != nil {
		return "", err
	}
	s, err := netip.ParseAddrPort(localAddr)
	if err != nil {
		return "", err
	}
	unmap := func(a netip.AddrPort) netip.AddrPort { return netip.AddrPortFrom(a.Addr().Unmap(), a.Port()) }
	uid, err := UID(unmap(c), unmap(s))
	if err != nil {
		return "", err
	}
	return Username(uid)
}
