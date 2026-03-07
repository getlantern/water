// echoserver is a standalone TCP echo server for remote benchmarking.
// Any data received on a connection is echoed back immediately via io.Copy.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	port := flag.Int("port", 8080, "TCP port to listen on")
	flag.Parse()

	addr := fmt.Sprintf(":%d", *port)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen %s: %v", addr, err)
	}
	log.Printf("echo server listening on %s", addr)

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go handle(conn)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down")
	lis.Close()
}

func handle(c net.Conn) {
	defer c.Close()
	io.Copy(c, c)
}
