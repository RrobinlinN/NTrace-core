//go:build linux

package internal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

// pingConn is an unprivileged replacement for a raw ICMP socket. It wraps a
// Linux "ping socket" (SOCK_DGRAM + IPPROTO_ICMP[V6]) and presents the same
// stream of ICMP messages a raw socket would: echo replies are read normally,
// while Time Exceeded / Unreachable errors are taken from the socket error
// queue and rebuilt into wire format.
//
// Embedding *net.UDPConn keeps the socket usable with x/net ipv4/ipv6
// PacketConn (TTL, TOS, hop limit) and with syscall.Conn based helpers.
type pingConn struct {
	*net.UDPConn
	ipVersion int
	localIP   net.IP
	raw       syscall.RawConn
	// echoID is the identifier the tracer put in its last echo request, or -1.
	echoID atomic.Int32
}

func unprivilegedICMPFamily(network string) (ipVersion, family, proto int, ok bool) {
	switch network {
	case "ip4:icmp", "ip4:1":
		return 4, unix.AF_INET, unix.IPPROTO_ICMP, true
	case "ip6:ipv6-icmp", "ip6:58":
		return 6, unix.AF_INET6, unix.IPPROTO_ICMPV6, true
	default:
		return 0, 0, 0, false
	}
}

// listenUnprivilegedICMP opens a ping socket when the raw socket was refused
// for lack of privilege. It reports false when no fallback applies or the
// kernel also refuses ping sockets (net.ipv4.ping_group_range).
func listenUnprivilegedICMP(network, laddr string, cause error) (net.PacketConn, bool) {
	if !errors.Is(cause, os.ErrPermission) {
		return nil, false
	}
	ipVersion, family, proto, ok := unprivilegedICMPFamily(network)
	if !ok {
		return nil, false
	}
	conn, err := newPingConn(ipVersion, family, proto, net.ParseIP(laddr))
	if err != nil {
		return nil, false
	}
	return conn, true
}

func newPingConn(ipVersion, family, proto int, localIP net.IP) (*pingConn, error) {
	fd, err := unix.Socket(family, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, proto)
	if err != nil {
		return nil, fmt.Errorf("open ping socket: %w", err)
	}
	if err := configurePingSocket(fd, ipVersion, localIP); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}

	file := os.NewFile(uintptr(fd), "nexttrace-ping")
	packetConn, err := net.FilePacketConn(file)
	_ = file.Close()
	if err != nil {
		return nil, fmt.Errorf("wrap ping socket: %w", err)
	}
	udpConn, ok := packetConn.(*net.UDPConn)
	if !ok {
		_ = packetConn.Close()
		return nil, fmt.Errorf("ping socket wrapped as unexpected type %T", packetConn)
	}
	raw, err := udpConn.SyscallConn()
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("ping socket SyscallConn: %w", err)
	}

	conn := &pingConn{UDPConn: udpConn, ipVersion: ipVersion, localIP: localIP, raw: raw}
	conn.echoID.Store(-1)
	return conn, nil
}

func configurePingSocket(fd, ipVersion int, localIP net.IP) error {
	if ipVersion == 4 {
		if err := unix.SetsockoptInt(fd, unix.SOL_IP, unix.IP_RECVERR, 1); err != nil {
			return fmt.Errorf("enable IP_RECVERR: %w", err)
		}
		if v4 := localIP.To4(); v4 != nil && !v4.IsUnspecified() {
			sa := &unix.SockaddrInet4{}
			copy(sa.Addr[:], v4)
			if err := unix.Bind(fd, sa); err != nil {
				return fmt.Errorf("bind ping socket to %s: %w", localIP, err)
			}
		}
		return nil
	}

	if err := unix.SetsockoptInt(fd, unix.SOL_IPV6, unix.IPV6_RECVERR, 1); err != nil {
		return fmt.Errorf("enable IPV6_RECVERR: %w", err)
	}
	if v6 := localIP.To16(); v6 != nil && localIP.To4() == nil && !v6.IsUnspecified() {
		sa := &unix.SockaddrInet6{}
		copy(sa.Addr[:], v6)
		if err := unix.Bind(fd, sa); err != nil {
			return fmt.Errorf("bind ping socket to %s: %w", localIP, err)
		}
	}
	return nil
}

// isPendingICMPErrno reports whether err is a socket error left pending by an
// ICMP message that the error queue already describes. Such an error is
// returned once by the next syscall and says nothing about that syscall.
func isPendingICMPErrno(err error) bool {
	for _, errno := range []syscall.Errno{
		syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.ECONNREFUSED,
		syscall.EPROTO, syscall.EHOSTDOWN, syscall.ENOPROTOOPT, syscall.ENONET,
	} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

func (c *pingConn) echoRequestType() byte {
	if c.ipVersion == 6 {
		return 128
	}
	return 8
}

func (c *pingConn) echoReplyType() byte {
	if c.ipVersion == 6 {
		return 129
	}
	return 0
}

func (c *pingConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	var ip net.IP
	var zone string
	switch a := addr.(type) {
	case *net.IPAddr:
		ip, zone = a.IP, a.Zone
	case *net.UDPAddr:
		ip, zone = a.IP, a.Zone
	default:
		return 0, fmt.Errorf("ping socket: unsupported address type %T", addr)
	}
	if len(b) >= 6 && b[0] == c.echoRequestType() {
		c.echoID.Store(int32(binary.BigEndian.Uint16(b[4:6])))
	}

	dst := &net.UDPAddr{IP: ip, Zone: zone}
	var (
		n   int
		err error
	)
	// A pending ICMP error is reported by (and cleared from) the next send, so
	// retry a few times before treating the failure as real.
	for attempt := 0; attempt < 4; attempt++ {
		n, err = c.UDPConn.WriteTo(b, dst)
		if err == nil || !isPendingICMPErrno(err) {
			break
		}
	}
	return n, err
}

func (c *pingConn) ReadFrom(b []byte) (int, net.Addr, error) {
	var (
		n     int
		peer  net.Addr
		opErr error
	)
	err := c.raw.Read(func(fd uintptr) bool {
		for {
			var got bool
			n, peer, got, opErr = c.recvQueuedError(int(fd), b)
			if got || opErr != nil {
				return true
			}
			n, peer, opErr = c.recvReply(int(fd), b)
			switch {
			case opErr == nil:
				return true
			case errors.Is(opErr, unix.EAGAIN):
				opErr = nil
				return false
			case errors.Is(opErr, unix.EINTR), isPendingICMPErrno(opErr):
				continue
			default:
				return true
			}
		}
	})
	if err != nil {
		return 0, nil, err
	}
	if opErr != nil {
		return 0, nil, &net.OpError{Op: "read", Net: "ip", Err: opErr}
	}
	return n, peer, nil
}

// recvQueuedError takes one ICMP error off the socket error queue and writes
// the rebuilt ICMP message into b. got is false when the queue is empty.
func (c *pingConn) recvQueuedError(fd int, b []byte) (n int, peer net.Addr, got bool, err error) {
	var (
		quoted [2048]byte
		oob    [512]byte
	)
	for {
		qn, oobn, _, from, rerr := unix.Recvmsg(fd, quoted[:], oob[:], unix.MSG_ERRQUEUE|unix.MSG_DONTWAIT)
		if rerr != nil {
			if errors.Is(rerr, unix.EINTR) {
				continue
			}
			if errors.Is(rerr, unix.EAGAIN) {
				return 0, nil, false, nil
			}
			return 0, nil, false, rerr
		}
		ee, ok := parseRecvErrControl(c.ipVersion, oob[:oobn])
		if !ok {
			// Locally generated or foreign-family error: skip it.
			continue
		}
		patchICMPEchoID(quoted[:qn], int(c.echoID.Load()))
		msg := buildSyntheticICMPError(c.ipVersion, ee, c.quotedIPHeader(from, qn), quoted[:qn])
		return copy(b, msg), &net.IPAddr{IP: ee.Offender}, true, nil
	}
}

// quotedIPHeader rebuilds the IP header of the echo request the error refers
// to. from is the original destination as reported by the kernel.
func (c *pingConn) quotedIPHeader(from unix.Sockaddr, payloadLen int) []byte {
	switch sa := from.(type) {
	case *unix.SockaddrInet4:
		return ipv4QuotedHeader(c.localIP, net.IP(sa.Addr[:]), unix.IPPROTO_ICMP, 0, 0, 1, payloadLen)
	case *unix.SockaddrInet6:
		return ipv6QuotedHeader(c.localIP, net.IP(sa.Addr[:]), unix.IPPROTO_ICMPV6, 0, 1, payloadLen)
	}
	if c.ipVersion == 6 {
		return ipv6QuotedHeader(c.localIP, nil, unix.IPPROTO_ICMPV6, 0, 1, payloadLen)
	}
	return ipv4QuotedHeader(c.localIP, nil, unix.IPPROTO_ICMP, 0, 0, 1, payloadLen)
}

func (c *pingConn) recvReply(fd int, b []byte) (int, net.Addr, error) {
	n, _, _, from, err := unix.Recvmsg(fd, b, nil, unix.MSG_DONTWAIT)
	if err != nil {
		return 0, nil, err
	}
	if n >= 6 && b[0] == c.echoReplyType() {
		patchICMPEchoID(b[:n], int(c.echoID.Load()))
	}
	switch sa := from.(type) {
	case *unix.SockaddrInet4:
		return n, &net.IPAddr{IP: append(net.IP(nil), sa.Addr[:]...)}, nil
	case *unix.SockaddrInet6:
		return n, &net.IPAddr{IP: append(net.IP(nil), sa.Addr[:]...)}, nil
	}
	return n, &net.IPAddr{}, nil
}

// parseRecvErrControl finds the IP_RECVERR / IPV6_RECVERR control message in
// oob and decodes it.
func parseRecvErrControl(ipVersion int, oob []byte) (sockExtendedErr, bool) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return sockExtendedErr{}, false
	}
	level, typ := int32(unix.SOL_IP), int32(unix.IP_RECVERR)
	if ipVersion == 6 {
		level, typ = unix.SOL_IPV6, unix.IPV6_RECVERR
	}
	for _, m := range msgs {
		if m.Header.Level == level && m.Header.Type == typ {
			return parseSockExtendedErr(ipVersion, m.Data)
		}
	}
	return sockExtendedErr{}, false
}
