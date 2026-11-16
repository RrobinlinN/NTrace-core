//go:build !linux

package internal

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/google/gopacket/layers"
)

// Unprivileged probing through the socket error queue is Linux/Android only.

type unprivUDP struct{}

func (u *unprivUDP) send(context.Context, ipLayer, *layers.UDP, []byte) (time.Time, error) {
	return time.Time{}, errors.New("unprivileged UDP probing is not supported on this platform")
}

func listenUDPErrorSource(s *UDPSpec, network string) (net.PacketConn, error) {
	return net.ListenPacket(network, s.SrcIP.String())
}

func listenUnprivilegedICMP(string, string, error) (net.PacketConn, bool) {
	return nil, false
}
