//go:build linux

package internal

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/google/gopacket/layers"
	"golang.org/x/sys/unix"
)

// unprivUDPProbeLifetime bounds how long a probe socket waits for an ICMP
// error before it is closed. It only needs to outlast the tracer's timeout.
const unprivUDPProbeLifetime = 10 * time.Second

// unprivUDP sends UDP probes without raw sockets. Every probe gets its own
// kernel-addressed UDP socket with IP_RECVERR enabled, so the ICMP error for
// that probe comes back on that socket's error queue and identifies the probe
// by itself. The errors are rebuilt into wire-format ICMP messages and handed
// to the tracer through a virtual PacketConn that stands in for the raw ICMP
// listener.
//
// The on-wire source port is chosen by the kernel per probe; the tracer's own
// source port is only used in the rebuilt quote so its matching keeps working.
type unprivUDP struct {
	ipVersion int
	srcIP     net.IP
	dstIP     net.IP

	msgs      chan unprivICMPMessage
	closed    chan struct{}
	closeOnce sync.Once

	mu     sync.Mutex
	probes map[*os.File]struct{}
	wg     sync.WaitGroup
}

type unprivICMPMessage struct {
	data []byte
	peer net.Addr
}

func newUnprivUDP(ipVersion int, srcIP, dstIP net.IP) *unprivUDP {
	return &unprivUDP{
		ipVersion: ipVersion,
		srcIP:     srcIP,
		dstIP:     dstIP,
		msgs:      make(chan unprivICMPMessage, 256),
		closed:    make(chan struct{}),
		probes:    make(map[*os.File]struct{}),
	}
}

// listenUDPErrorSource opens the ICMP listener for UDP probing. When the raw
// socket is refused for lack of privilege it switches the spec to the
// unprivileged per-probe-socket path instead.
func listenUDPErrorSource(s *UDPSpec, network string) (net.PacketConn, error) {
	conn, err := net.ListenPacket(network, s.SrcIP.String())
	if err == nil {
		return conn, nil
	}
	if !errors.Is(err, os.ErrPermission) {
		return nil, err
	}
	s.unpriv = newUnprivUDP(s.IPVersion, s.SrcIP, s.DstIP)
	return &unprivUDPConn{u: s.unpriv}, nil
}

func (u *unprivUDP) close() {
	u.closeOnce.Do(func() {
		close(u.closed)
		u.mu.Lock()
		for f := range u.probes {
			_ = f.Close()
		}
		u.mu.Unlock()
	})
	u.wg.Wait()
}

func (u *unprivUDP) track(f *os.File) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	select {
	case <-u.closed:
		return false
	default:
	}
	u.probes[f] = struct{}{}
	u.wg.Add(1)
	return true
}

func (u *unprivUDP) release(f *os.File) {
	u.mu.Lock()
	delete(u.probes, f)
	u.mu.Unlock()
	_ = f.Close()
	u.wg.Done()
}

func (u *unprivUDP) send(ctx context.Context, ipHdr ipLayer, udpHdr *layers.UDP, payload []byte) (time.Time, error) {
	if udpHdr == nil {
		return time.Time{}, errors.New("SendUDP: nil UDP header")
	}
	if err := udpHdr.SetNetworkLayerForChecksum(ipHdr); err != nil {
		return time.Time{}, fmt.Errorf("SetNetworkLayerForChecksum: %w", err)
	}
	// Serialize the tracer's UDP header: for IPv6 its checksum carries the
	// probe sequence, so the quote must reproduce it exactly.
	segment, err := serializeUDPPacket(payload, udpHdr)
	if err != nil {
		return time.Time{}, err
	}
	if len(segment) < 8 {
		return time.Time{}, errors.New("SendUDP: short UDP segment")
	}

	fd, quotedIP, err := u.openProbeSocket(ipHdr, len(segment))
	if err != nil {
		return time.Time{}, err
	}
	file := os.NewFile(uintptr(fd), "nexttrace-udp-probe")
	if !u.track(file) {
		_ = file.Close()
		return time.Time{}, context.Canceled
	}

	quoted := append(quotedIP, segment[:8]...)
	raw, err := file.SyscallConn()
	if err != nil {
		u.release(file)
		return time.Time{}, err
	}

	dst := u.sockaddr(int(udpHdr.DstPort))
	var sendErr error
	start := time.Now()
	if err := raw.Write(func(fd uintptr) bool {
		sendErr = unix.Sendto(int(fd), payload, 0, dst)
		return !errors.Is(sendErr, unix.EAGAIN)
	}); err != nil {
		sendErr = err
	}
	if sendErr != nil {
		u.release(file)
		return time.Time{}, sendErr
	}

	go u.await(ctx, file, raw, quoted)
	return start, nil
}

// openProbeSocket creates a UDP socket configured like the probe described by
// ipHdr and returns it with the IP header a router would quote.
func (u *unprivUDP) openProbeSocket(ipHdr ipLayer, segmentLen int) (int, []byte, error) {
	switch ip := ipHdr.(type) {
	case *layers.IPv4:
		if ip == nil {
			return -1, nil, errors.New("SendUDP: expect *layers.IPv4 when s.IPVersion==4")
		}
		fd, err := newProbeSocket(unix.AF_INET, [][3]int{
			{unix.SOL_IP, unix.IP_RECVERR, 1},
			{unix.SOL_IP, unix.IP_TTL, int(ip.TTL)},
			{unix.SOL_IP, unix.IP_TOS, int(ip.TOS)},
		})
		if err != nil {
			return -1, nil, err
		}
		if v4 := u.srcIP.To4(); v4 != nil && !v4.IsUnspecified() {
			sa := &unix.SockaddrInet4{}
			copy(sa.Addr[:], v4)
			if err := unix.Bind(fd, sa); err != nil {
				_ = unix.Close(fd)
				return -1, nil, fmt.Errorf("bind UDP probe socket to %s: %w", u.srcIP, err)
			}
		}
		quoted := ipv4QuotedHeader(u.srcIP, u.dstIP, unix.IPPROTO_UDP, ip.Id, ip.TOS, 1, segmentLen)
		return fd, quoted, nil
	case *layers.IPv6:
		if ip == nil {
			return -1, nil, errors.New("SendUDP: expect *layers.IPv6 when s.IPVersion==6")
		}
		fd, err := newProbeSocket(unix.AF_INET6, [][3]int{
			{unix.SOL_IPV6, unix.IPV6_RECVERR, 1},
			{unix.SOL_IPV6, unix.IPV6_UNICAST_HOPS, int(ip.HopLimit)},
			{unix.SOL_IPV6, unix.IPV6_TCLASS, int(ip.TrafficClass)},
		})
		if err != nil {
			return -1, nil, err
		}
		if v6 := u.srcIP.To16(); v6 != nil && u.srcIP.To4() == nil && !v6.IsUnspecified() {
			sa := &unix.SockaddrInet6{}
			copy(sa.Addr[:], v6)
			if err := unix.Bind(fd, sa); err != nil {
				_ = unix.Close(fd)
				return -1, nil, fmt.Errorf("bind UDP probe socket to %s: %w", u.srcIP, err)
			}
		}
		quoted := ipv6QuotedHeader(u.srcIP, u.dstIP, unix.IPPROTO_UDP, ip.TrafficClass, 1, segmentLen)
		return fd, quoted, nil
	default:
		return -1, nil, fmt.Errorf("SendUDP: unsupported IP layer %T", ipHdr)
	}
}

func newProbeSocket(family int, opts [][3]int) (int, error) {
	fd, err := unix.Socket(family, unix.SOCK_DGRAM|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, unix.IPPROTO_UDP)
	if err != nil {
		return -1, fmt.Errorf("open UDP probe socket: %w", err)
	}
	for _, opt := range opts {
		if err := unix.SetsockoptInt(fd, opt[0], opt[1], opt[2]); err != nil {
			_ = unix.Close(fd)
			wrapped := fmt.Errorf("setsockopt(level=%d, name=%d, value=%d): %w", opt[0], opt[1], opt[2], err)
			if opt[1] == unix.IP_TOS || opt[1] == unix.IPV6_TCLASS {
				return -1, &InitializationError{Err: wrapped}
			}
			return -1, wrapped
		}
	}
	return fd, nil
}

func (u *unprivUDP) sockaddr(port int) unix.Sockaddr {
	if u.ipVersion == 6 {
		sa := &unix.SockaddrInet6{Port: port}
		copy(sa.Addr[:], u.dstIP.To16())
		return sa
	}
	sa := &unix.SockaddrInet4{Port: port}
	copy(sa.Addr[:], u.dstIP.To4())
	return sa
}

// await waits for the ICMP error of one probe and forwards it to the tracer.
func (u *unprivUDP) await(ctx context.Context, file *os.File, raw syscall.RawConn, quoted []byte) {
	defer u.release(file)

	stop := context.AfterFunc(ctx, func() { _ = file.SetReadDeadline(time.Unix(1, 0)) })
	defer stop()
	_ = file.SetReadDeadline(time.Now().Add(unprivUDPProbeLifetime))

	var (
		payload [2048]byte
		oob     [512]byte
		n, oobn int
		recvErr error
		ee      sockExtendedErr
		ok      bool
	)
	for !ok {
		err := raw.Read(func(fd uintptr) bool {
			n, oobn, _, _, recvErr = unix.Recvmsg(int(fd), payload[:], oob[:], unix.MSG_ERRQUEUE|unix.MSG_DONTWAIT)
			return !errors.Is(recvErr, unix.EAGAIN)
		})
		if err != nil {
			return
		}
		if recvErr != nil {
			if errors.Is(recvErr, unix.EINTR) {
				continue
			}
			return
		}
		// Skip locally generated errors and keep waiting for the ICMP one.
		ee, ok = parseRecvErrControl(u.ipVersion, oob[:oobn])
	}

	msg := unprivICMPMessage{
		data: buildSyntheticICMPError(u.ipVersion, ee, quoted, payload[:n]),
		peer: &net.IPAddr{IP: ee.Offender},
	}
	select {
	case u.msgs <- msg:
	case <-u.closed:
	}
}

// unprivUDPConn is the virtual ICMP listener fed by unprivUDP.
type unprivUDPConn struct {
	u *unprivUDP
}

func (c *unprivUDPConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case msg := <-c.u.msgs:
		return copy(b, msg.data), msg.peer, nil
	case <-c.u.closed:
		return 0, nil, net.ErrClosed
	}
}

func (c *unprivUDPConn) WriteTo([]byte, net.Addr) (int, error) {
	return 0, errors.New("unprivileged ICMP listener is receive-only")
}

func (c *unprivUDPConn) Close() error {
	c.u.close()
	return nil
}

func (c *unprivUDPConn) LocalAddr() net.Addr              { return &net.IPAddr{IP: c.u.srcIP} }
func (c *unprivUDPConn) SetDeadline(time.Time) error      { return nil }
func (c *unprivUDPConn) SetReadDeadline(time.Time) error  { return nil }
func (c *unprivUDPConn) SetWriteDeadline(time.Time) error { return nil }
