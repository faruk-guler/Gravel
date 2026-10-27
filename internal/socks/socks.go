// Package socks implements the server side of SOCKS5 (RFC 1928) CONNECT,
// enough to back Gravel's dynamic (-D) tunnels: a browser or curl talks the
// protocol with Serve on the local port, and every request's own destination
// is dialled through the SSH connection.
package socks

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// handshakeTimeout bounds how long a client may take to send its greeting and
// CONNECT request. A dynamic (-D) tunnel can be bound to the LAN, so a socket
// that is accepted and then never completes the handshake would otherwise pin a
// goroutine - and the SSH session behind it - for as long as the client keeps
// the connection open. A var so the tests do not have to wait out 15 seconds.
var handshakeTimeout = 15 * time.Second

// deadlineSetter is satisfied by every real net.Conn; tests can pass a pipe.
type deadlineSetter interface {
	SetDeadline(time.Time) error
}

// setDeadline clears (zero time) or arms the handshake timer. Failures are
// ignored: a connection without deadline support still works, just unguarded.
func setDeadline(conn net.Conn, t time.Time) {
	if ds, ok := conn.(deadlineSetter); ok {
		_ = ds.SetDeadline(t)
	}
}

const (
	version      = 0x05
	cmdConnect   = 0x01
	atypIPv4     = 0x01
	atypDomain   = 0x03
	atypIPv6     = 0x04
	noAuth       = 0x00
	noAcceptable = 0xff
)

// SOCKS reply status codes we surface to clients (RFC 1928 section 3).
const (
	statusGranted        = 0x00
	statusFailure        = 0x01
	statusRefused        = 0x02
	statusNetworkUnreach = 0x03
	statusHostUnreach    = 0x04
	statusRefusedCmd     = 0x07
	statusBadAtyp        = 0x08
)

// Dial opens the outbound connection for a validated target. It is supplied by
// the caller - for a tunnel that is ssh.Client.Dial, so the target is reached
// through the SSH link rather than the local network.
type Dial func(network, addr string) (net.Conn, error)

// Serve performs the SOCKS5 handshake on conn, dials the requested target
// through dial and relays bytes in both directions. conn is always closed
// before Serve returns; the accepted remote connection too.
func Serve(conn net.Conn, dial Dial) error {
	defer conn.Close()
	setDeadline(conn, time.Now().Add(handshakeTimeout))
	defer setDeadline(conn, time.Time{})

	greet := make([]byte, 2)
	if _, err := io.ReadFull(conn, greet); err != nil {
		return err
	}
	if greet[0] != version || greet[1] == 0 {
		return errors.New("socks: bad greeting")
	}
	methods := make([]byte, greet[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	// The tunnel adds no authentication layer of its own - the SSH link in
	// front of it already is the authentication - so only "no auth" is offered.
	if !hasMethod(methods, noAuth) {
		_, _ = conn.Write([]byte{version, noAcceptable})
		return errors.New("socks: no acceptable auth method")
	}
	if _, err := conn.Write([]byte{version, noAuth}); err != nil {
		return err
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return err
	}
	if head[0] != version {
		return errors.New("socks: bad request version")
	}
	// The address is read even for unsupported commands so the stream stays
	// framed while we decide to reject.
	host, err := readAddr(conn, head[3])
	if err != nil {
		_ = reply(conn, statusBadAtyp)
		return err
	}
	if head[1] != cmdConnect {
		_ = reply(conn, statusRefusedCmd)
		return fmt.Errorf("socks: command %d not supported", head[1])
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(conn, pb); err != nil {
		return err
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb))))

	// The handshake is complete, so the guard comes off *before* dialling: a
	// legitimate but slow target behind the SSH link must not be cut off, and the
	// established relay is allowed to stay quiet for hours.
	setDeadline(conn, time.Time{})

	remote, err := dial("tcp", target)
	if err != nil {
		_ = reply(conn, statusHostUnreach)
		return fmt.Errorf("socks: dial %s: %w", target, err)
	}
	defer remote.Close()
	if err := reply(conn, statusGranted); err != nil {
		return err
	}

	go func() { _, _ = io.Copy(remote, conn); remote.Close() }()
	_, _ = io.Copy(conn, remote)
	return nil
}

func hasMethod(methods []byte, want byte) bool {
	for _, m := range methods {
		if m == want {
			return true
		}
	}
	return false
}

// readAddr decodes one ATYP-dependent address without its port.
func readAddr(r io.Reader, atyp byte) (string, error) {
	switch atyp {
	case atypIPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case atypIPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case atypDomain:
		l := make([]byte, 1)
		if _, err := io.ReadFull(r, l); err != nil {
			return "", err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("socks: unsupported address type %d", atyp)
	}
}

// reply writes a SOCKS5 response with the given status and a zero bound
// address; CONNECT clients ignore the bound field through a proxy.
func reply(w io.Writer, status byte) error {
	_, err := w.Write([]byte{version, status, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}
