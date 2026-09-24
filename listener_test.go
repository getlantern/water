package water_test

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/refraction-networking/water"
	_ "github.com/refraction-networking/water/transport/v1"
)

// ExampleListener demonstrates how to use water.Listener.
//
// This example is expected to demonstrate how to use the LATEST version of
// W.A.T.E.R. API, while other older examples could be found under transport/vX,
// where X is the version number (e.g. v0, v1, etc.).
//
// It is worth noting that unless the W.A.T.E.R. API changes, the version upgrade
// does not bring any essential changes to this example other than the import
// path and wasm file path.
func ExampleListener() {
	config := &water.Config{
		TransportModuleBin:  wasmReverse,
		ModuleConfigFactory: water.NewWazeroModuleConfigFactory(),
	}

	waterListener, err := config.ListenContext(context.Background(), "tcp", "localhost:0")
	if err != nil {
		panic(err)
	}
	defer waterListener.Close() // skipcq: GO-S2307

	tcpConn, err := net.Dial("tcp", waterListener.Addr().String())
	if err != nil {
		panic(err)
	}
	defer tcpConn.Close() // skipcq: GO-S2307

	waterConn, err := waterListener.Accept()
	if err != nil {
		panic(err)
	}
	defer waterConn.Close() // skipcq: GO-S2307

	var msg = []byte("hello")
	n, err := tcpConn.Write(msg)
	if err != nil {
		panic(err)
	}
	if n != len(msg) {
		panic("short write")
	}

	buf := make([]byte, 1024)
	n, err = waterConn.Read(buf)
	if err != nil {
		panic(err)
	}
	if n != len(msg) {
		panic("short read")
	}

	if err := waterConn.Close(); err != nil {
		panic(err)
	}

	fmt.Println(string(buf[:n]))
	// Output: olleh
}

// ListenContext opens the network listener before building the WATER
// listener, so a construction failure must close it rather than leak the port.
func TestListenContextClosesSocketOnError(t *testing.T) {
	probe, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := probe.Addr().String()
	probe.Close()

	config := &water.Config{TransportModuleBin: []byte("not a wasm module")}
	if _, err := config.ListenContext(context.Background(), "tcp", addr); err == nil {
		t.Fatal("ListenContext should fail for an invalid module")
	}

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port still held after failed ListenContext: %v", err)
	}
	lis.Close()
}
