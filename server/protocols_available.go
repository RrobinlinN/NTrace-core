package server

import (
	"errors"
	"net"
	"os"
	"runtime"
	"sync"
)

// availableProtocols lists the probe protocols this process can actually use,
// so the Web UI does not offer modes that can only fail. It matters for the
// mobile shell apps, which usually run without raw-socket privilege:
//   - Linux/Android without CAP_NET_RAW: ICMP and UDP work through the
//     unprivileged fallback, TCP still needs a raw socket.
//   - iOS: there is no libpcap, so only ICMP is implemented.
//
// Requests for other protocols are still accepted and fail with their own error.
var availableProtocols = sync.OnceValue(func() []string {
	switch runtime.GOOS {
	case "linux", "android":
		conn, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
		if err == nil {
			_ = conn.Close()
			return supportedProtocols
		}
		if errors.Is(err, os.ErrPermission) {
			return []string{"icmp", "udp"}
		}
	case "ios":
		return []string{"icmp"}
	}
	return supportedProtocols
})
