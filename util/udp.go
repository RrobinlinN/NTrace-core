package util

import (
	"net"
	"runtime"
)

func newUDPResolver() *net.Resolver {
	return &net.Resolver{
		// 指定使用 Go Build-in 的 DNS Resolver 来解析
		PreferGo: preferGoResolver(runtime.GOOS),
	}
}

// preferGoResolver reports whether the pure-Go DNS client can be used on goos.
// It learns the name servers from /etc/resolv.conf, which Android and iOS do
// not have: there it falls back to localhost:53 and every lookup fails with
// "connection refused". Those systems must go through the libc resolver.
func preferGoResolver(goos string) bool {
	return goos != "android" && goos != "ios"
}
