//go:build !unix

package socket

import "net"

// ConnPair returns a pair of connected net.Conn.
// On non-Unix platforms, this falls back to TCPConnPair.
func ConnPair() (c1, c2 net.Conn, err error) {
	tc1, tc2, err := TCPConnPair()
	return tc1, tc2, err
}
