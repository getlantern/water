//go:build !unix

package socket

import (
	"log"
	"net"
)

// ConnPair returns a pair of connected net.Conn.
// On non-Unix platforms, this falls back to TCPConnPair.
func ConnPair() (c1, c2 net.Conn, err error) {
	tc1, tc2, err := TCPConnPair()
	if tc1 != nil && tc2 != nil {
		if err != nil {
			log.Printf("socket.ConnPair: TCPConnPair returned non-fatal error with usable connections: %v", err)
		}
		err = nil
	}
	return tc1, tc2, err
}
