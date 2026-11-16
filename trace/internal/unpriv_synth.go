package internal

import (
	"encoding/binary"
	"net"
)

// Unprivileged probing on Linux/Android cannot open raw sockets. The kernel
// instead reports ICMP errors through the socket error queue (IP_RECVERR /
// IPV6_RECVERR), stripped of the outer ICMP header and the quoted IP header.
// The helpers in this file rebuild a wire-format ICMP error from that report
// so the shared decoders keep working unchanged.

const (
	soEEOriginICMP  = 2
	soEEOriginICMP6 = 3

	sockExtendedErrLen = 16
)

// sockExtendedErr mirrors struct sock_extended_err plus the offender address
// that follows it in the control message.
type sockExtendedErr struct {
	Errno    uint32
	Origin   uint8
	Type     uint8
	Code     uint8
	Info     uint32
	Offender net.IP
}

// parseSockExtendedErr decodes the payload of an IP_RECVERR / IPV6_RECVERR
// control message. It reports false when the error did not originate from an
// ICMP message of the matching family or carries no offender address.
func parseSockExtendedErr(ipVersion int, data []byte) (sockExtendedErr, bool) {
	if len(data) < sockExtendedErrLen {
		return sockExtendedErr{}, false
	}
	ee := sockExtendedErr{
		Errno:  binary.NativeEndian.Uint32(data[0:4]),
		Origin: data[4],
		Type:   data[5],
		Code:   data[6],
		Info:   binary.NativeEndian.Uint32(data[8:12]),
	}
	addr := data[sockExtendedErrLen:]
	switch ipVersion {
	case 4:
		// struct sockaddr_in: family(2) port(2) addr(4)
		if ee.Origin != soEEOriginICMP || len(addr) < 8 {
			return sockExtendedErr{}, false
		}
		ee.Offender = append(net.IP(nil), addr[4:8]...)
	case 6:
		// struct sockaddr_in6: family(2) port(2) flowinfo(4) addr(16)
		if ee.Origin != soEEOriginICMP6 || len(addr) < 24 {
			return sockExtendedErr{}, false
		}
		ee.Offender = append(net.IP(nil), addr[8:24]...)
	default:
		return sockExtendedErr{}, false
	}
	return ee, true
}

// buildSyntheticICMPError returns an ICMP error message (starting at the ICMP
// header) whose body is the concatenation of the quoted parts.
func buildSyntheticICMPError(ipVersion int, ee sockExtendedErr, quoted ...[]byte) []byte {
	size := 8
	for _, part := range quoted {
		size += len(part)
	}
	msg := make([]byte, 8, size)
	msg[0] = ee.Type
	msg[1] = ee.Code
	switch {
	case ipVersion == 4 && ee.Type == 3 && ee.Code == 4:
		// Fragmentation needed: next-hop MTU lives in the low 16 bits.
		binary.BigEndian.PutUint16(msg[6:8], uint16(ee.Info))
	case ipVersion == 6 && ee.Type == 2:
		// Packet Too Big: 32-bit MTU.
		binary.BigEndian.PutUint32(msg[4:8], ee.Info)
	}
	for _, part := range quoted {
		msg = append(msg, part...)
	}
	return msg
}

// ipv4QuotedHeader builds the 20-byte IPv4 header of the original probe as a
// router would quote it. payloadLen is the length of everything after it.
func ipv4QuotedHeader(src, dst net.IP, protocol uint8, id uint16, tos uint8, ttl uint8, payloadLen int) []byte {
	hdr := make([]byte, 20)
	hdr[0] = 0x45
	hdr[1] = tos
	binary.BigEndian.PutUint16(hdr[2:4], uint16(20+payloadLen))
	binary.BigEndian.PutUint16(hdr[4:6], id)
	hdr[8] = ttl
	hdr[9] = protocol
	if v4 := src.To4(); v4 != nil {
		copy(hdr[12:16], v4)
	}
	if v4 := dst.To4(); v4 != nil {
		copy(hdr[16:20], v4)
	}
	return hdr
}

// ipv6QuotedHeader builds the 40-byte IPv6 header of the original probe.
func ipv6QuotedHeader(src, dst net.IP, nextHeader uint8, trafficClass uint8, hopLimit uint8, payloadLen int) []byte {
	hdr := make([]byte, 40)
	hdr[0] = 0x60 | trafficClass>>4
	hdr[1] = trafficClass << 4
	binary.BigEndian.PutUint16(hdr[4:6], uint16(payloadLen))
	hdr[6] = nextHeader
	hdr[7] = hopLimit
	if v6 := src.To16(); v6 != nil {
		copy(hdr[8:24], v6)
	}
	if v6 := dst.To16(); v6 != nil {
		copy(hdr[24:40], v6)
	}
	return hdr
}

// patchICMPEchoID overwrites the identifier of an ICMP echo header in place.
// Ping sockets replace the identifier with a kernel-chosen one, so replies and
// quoted requests must be mapped back to the identifier the tracer expects.
func patchICMPEchoID(icmpHeader []byte, id int) {
	if id < 0 || len(icmpHeader) < 6 {
		return
	}
	binary.BigEndian.PutUint16(icmpHeader[4:6], uint16(id))
}
