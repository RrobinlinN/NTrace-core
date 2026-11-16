package internal

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/nxtrace/NTrace-core/util"
)

func recvErrPayload(origin, typ, code uint8, info uint32, sockaddr []byte) []byte {
	data := make([]byte, sockExtendedErrLen, sockExtendedErrLen+len(sockaddr))
	binary.NativeEndian.PutUint32(data[0:4], 113)
	data[4], data[5], data[6] = origin, typ, code
	binary.NativeEndian.PutUint32(data[8:12], info)
	return append(data, sockaddr...)
}

func TestParseSockExtendedErr(t *testing.T) {
	sin := []byte{2, 0, 0, 0, 10, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}
	ee, ok := parseSockExtendedErr(4, recvErrPayload(soEEOriginICMP, 11, 0, 0, sin))
	if !ok || ee.Type != 11 || ee.Code != 0 || !ee.Offender.Equal(net.IPv4(10, 0, 0, 1)) {
		t.Fatalf("IPv4 parse = %+v, %v", ee, ok)
	}

	sin6 := make([]byte, 28)
	offender := net.ParseIP("2001:db8::1")
	copy(sin6[8:24], offender)
	ee, ok = parseSockExtendedErr(6, recvErrPayload(soEEOriginICMP6, 3, 0, 0, sin6))
	if !ok || ee.Type != 3 || !ee.Offender.Equal(offender) {
		t.Fatalf("IPv6 parse = %+v, %v", ee, ok)
	}

	// Locally generated errors (origin 1) and truncated payloads are rejected.
	if _, ok := parseSockExtendedErr(4, recvErrPayload(1, 11, 0, 0, sin)); ok {
		t.Fatal("local-origin error must be rejected")
	}
	if _, ok := parseSockExtendedErr(4, recvErrPayload(soEEOriginICMP, 11, 0, 0, nil)); ok {
		t.Fatal("error without offender must be rejected")
	}
}

// A rebuilt ICMP echo error must be accepted by the same decoders the raw
// socket path uses, and yield the tracer's identifier and sequence.
func TestSyntheticICMPEchoErrorDecodes(t *testing.T) {
	const echoID, seq = 0x1234, 0x0a01
	src, dst := net.IPv4(192, 0, 2, 1), net.IPv4(198, 51, 100, 7)

	// Quoted echo header as the kernel returns it: identifier rewritten.
	quoted := []byte{8, 0, 0, 0, 0xbe, 0xef, byte(seq >> 8), byte(seq & 0xff)}
	patchICMPEchoID(quoted, echoID)
	ee := sockExtendedErr{Origin: soEEOriginICMP, Type: 11, Offender: net.IPv4(10, 0, 0, 1)}
	msg := buildSyntheticICMPError(4, ee,
		ipv4QuotedHeader(src, dst, 1, 0, 0, 1, len(quoted)), quoted)

	rm, ok := parseSocketICMPMessage(4, msg)
	if !ok {
		t.Fatal("synthetic message did not parse")
	}
	if got := classifySocketICMPResponse(4, rm, msg); got.Kind != ICMPResponseTransit {
		t.Fatalf("kind = %v, want transit", got.Kind)
	}
	data, ok := extractSocketICMPPayload(4, rm, dst)
	if !ok {
		t.Fatal("embedded destination did not match")
	}
	gotSeq, ok := extractEmbeddedICMPSeq(data, echoID)
	if !ok || gotSeq != seq {
		t.Fatalf("seq = %#x, %v; want %#x", gotSeq, ok, seq)
	}
}

func TestSyntheticUDPErrorDecodes(t *testing.T) {
	const srcPort, dstPort, seq = 40000, 33494, 0x0502
	src, dst := net.IPv4(192, 0, 2, 1), net.IPv4(198, 51, 100, 7)

	udp := make([]byte, 8)
	binary.BigEndian.PutUint16(udp[0:2], srcPort)
	binary.BigEndian.PutUint16(udp[2:4], dstPort)
	binary.BigEndian.PutUint16(udp[4:6], 8)
	ee := sockExtendedErr{Origin: soEEOriginICMP, Type: 3, Code: 3, Offender: dst}
	msg := buildSyntheticICMPError(4, ee,
		ipv4QuotedHeader(src, dst, 17, seq, 0, 1, len(udp)), udp)

	rm, ok := parseSocketICMPMessage(4, msg)
	if !ok {
		t.Fatal("synthetic message did not parse")
	}
	if got := classifySocketICMPResponse(4, rm, msg); got.Kind != ICMPResponsePortUnreachable {
		t.Fatalf("kind = %v, want port unreachable", got.Kind)
	}
	data, ok := extractSocketICMPPayload(4, rm, dst)
	if !ok {
		t.Fatal("embedded destination did not match")
	}
	if got, err := util.GetUDPSeq(data); err != nil || got != seq {
		t.Fatalf("GetUDPSeq = %#x, %v; want %#x", got, err, seq)
	}
	header, err := util.GetICMPResponsePayload(data)
	if err != nil {
		t.Fatalf("GetICMPResponsePayload: %v", err)
	}
	if sp, dp, err := util.GetUDPPorts(header); err != nil || sp != srcPort || dp != dstPort {
		t.Fatalf("ports = %d/%d, %v", sp, dp, err)
	}
}

func TestSyntheticICMPv6ErrorDecodes(t *testing.T) {
	const echoID, seq = 0x4321, 0x0301
	src, dst := net.ParseIP("2001:db8::10"), net.ParseIP("2001:db8:1::20")

	quoted := []byte{128, 0, 0, 0, 0, 0, byte(seq >> 8), byte(seq & 0xff)}
	patchICMPEchoID(quoted, echoID)
	ee := sockExtendedErr{Origin: soEEOriginICMP6, Type: 3, Offender: net.ParseIP("2001:db8::1")}
	msg := buildSyntheticICMPError(6, ee,
		ipv6QuotedHeader(src, dst, 58, 0, 1, len(quoted)), quoted)

	rm, ok := parseSocketICMPMessage(6, msg)
	if !ok {
		t.Fatal("synthetic message did not parse")
	}
	if got := classifySocketICMPResponse(6, rm, msg); got.Kind != ICMPResponseTransit {
		t.Fatalf("kind = %v, want transit", got.Kind)
	}
	data, ok := extractSocketICMPPayload(6, rm, dst)
	if !ok {
		t.Fatal("embedded destination did not match")
	}
	gotSeq, ok := extractEmbeddedICMPSeq(data, echoID)
	if !ok || gotSeq != seq {
		t.Fatalf("seq = %#x, %v; want %#x", gotSeq, ok, seq)
	}
}
