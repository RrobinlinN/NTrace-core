//go:build ios

package internal

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

// The Darwin TCP and UDP backends capture their own probes through libpcap,
// which Apple does not make available on iOS. Only ICMP (datagram ICMP socket,
// see icmp_darwin.go) works there, so these specs fail at initialization.

var errIOSProbeUnsupported = errors.New("this probe mode is not available on iOS (no libpcap); use ICMP")

type UDPSpec struct {
	IPVersion    int
	ICMPMode     int
	SrcIP        net.IP
	DstIP        net.IP
	DstPort      int
	SourceDevice string
	FWMark       uint32
	FWMarkSet    bool
	icmp         net.PacketConn
}

func (s *UDPSpec) InitUDP() error { return errIOSProbeUnsupported }

func (s *UDPSpec) Close() {
	if s.icmp != nil {
		_ = s.icmp.Close()
	}
}

func (s *UDPSpec) ListenOut(_ context.Context, _ chan struct{}, _ func(srcPort, seq, ttl int, start time.Time)) error {
	return errIOSProbeUnsupported
}

func (s *UDPSpec) ListenICMP(ctx context.Context, ready chan struct{}, onICMP func(msg ReceivedMessage, finish time.Time, data []byte)) error {
	return s.listenICMPSock(ctx, ready, onICMP)
}

func (s *UDPSpec) SendUDP(context.Context, gopacket.NetworkLayer, *layers.UDP, []byte) (time.Time, error) {
	return time.Time{}, errIOSProbeUnsupported
}

type TCPSpec struct {
	IPVersion    int
	ICMPMode     int
	SrcIP        net.IP
	DstIP        net.IP
	DstPort      int
	PktSize      int
	SourceDevice string
	FWMark       uint32
	FWMarkSet    bool
	icmp         net.PacketConn
}

func (s *TCPSpec) InitTCP() error { return errIOSProbeUnsupported }

func (s *TCPSpec) Close() {
	if s.icmp != nil {
		_ = s.icmp.Close()
	}
}

func (s *TCPSpec) ListenICMP(ctx context.Context, ready chan struct{}, onICMP func(msg ReceivedMessage, finish time.Time, data []byte)) error {
	return s.listenICMPSock(ctx, ready, onICMP)
}

func (s *TCPSpec) ListenTCP(_ context.Context, _ chan struct{}, _ func(srcPort, seq, ack int, peer net.Addr, finish time.Time)) error {
	return errIOSProbeUnsupported
}

func (s *TCPSpec) SendTCP(context.Context, gopacket.NetworkLayer, *layers.TCP, []byte) (time.Time, error) {
	return time.Time{}, errIOSProbeUnsupported
}

func checkTCPCapture(context.Context, *TCPSpec) []BackendCheck { return nil }
func checkUDPCapture(context.Context, *UDPSpec) []BackendCheck { return nil }
