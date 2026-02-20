//go:build unix

package socket

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// ConnPair returns a pair of connected net.Conn using a Unix domain
// socketpair. This avoids the overhead of TCP connection setup
// (listener, handshake, ephemeral port allocation) used by TCPConnPair.
func ConnPair() (c1, c2 net.Conn, err error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("syscall.Socketpair returned error: %w", err)
	}

	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])

	f1 := os.NewFile(uintptr(fds[0]), "connpair")
	f2 := os.NewFile(uintptr(fds[1]), "connpair")

	c1, err = net.FileConn(f1)
	f1.Close() // FileConn dups the fd, close the original
	if err != nil {
		f2.Close()
		return nil, nil, fmt.Errorf("net.FileConn returned error: %w", err)
	}

	c2, err = net.FileConn(f2)
	f2.Close()
	if err != nil {
		c1.Close()
		return nil, nil, fmt.Errorf("net.FileConn returned error: %w", err)
	}

	return c1, c2, nil
}
