package v1_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/refraction-networking/water"
)

func startEcho(tb testing.TB) net.Listener {
	tb.Helper()
	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		tb.Fatal(err)
	}
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { io.Copy(c, c); c.Close() }(c)
		}
	}()
	return lis
}

func roundtrip(tb testing.TB, conn net.Conn, msg []byte) {
	tb.Helper()
	if _, err := conn.Write(msg); err != nil {
		tb.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		tb.Fatal(err)
	}
	if !bytes.Equal(buf, msg) {
		tb.Fatalf("roundtrip mismatch: got %q, want %q", buf, msg)
	}
}

func forceGC() {
	for range 5 {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}

// A connection keeps its instance's shared runtime alive after the Dialer that
// made it is garbage collected; the runtime is only released once the Dialer is
// gone and must not close underneath the live connection.
func TestDialerGCKeepsLiveConn(t *testing.T) {
	echo := startEcho(t)
	defer echo.Close()

	dial := func() net.Conn {
		dialer, err := water.NewDialerWithContext(context.Background(), &water.Config{TransportModuleBin: wasmPlain})
		if err != nil {
			t.Fatal(err)
		}
		conn, err := dialer.DialContext(context.Background(), "tcp", echo.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}

	conn := dial()
	defer conn.Close()
	forceGC()
	roundtrip(t, conn, []byte("still alive after dialer GC"))
	conn.Close()

	conn2 := dial()
	defer conn2.Close()
	roundtrip(t, conn2, []byte("a fresh dialer still works"))
}

// A Dialer dropped right after DialContext starts can be finalized while the
// dial is still in flight, before its core has instantiated. The runtime must
// count that core as live rather than close underneath it.
func TestDialerGCDuringDial(t *testing.T) {
	echo := startEcho(t)
	defer echo.Close()

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()

	for i := range 30 {
		dialer, err := water.NewDialerWithContext(context.Background(), &water.Config{TransportModuleBin: wasmPlain})
		if err != nil {
			t.Fatal(err)
		}
		conn, err := dialer.DialContext(context.Background(), "tcp", echo.Addr().String())
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		roundtrip(t, conn, []byte("x"))
		conn.Close()
	}
}

// Canceling the context a Dialer was built with ends only the prewarmed
// connection, whose core inherits it. Connections dialed with their own
// contexts, and later dials, run on the shared runtime and must be unaffected.
func TestDialerConstructorCtxCancel(t *testing.T) {
	echo := startEcho(t)
	defer echo.Close()
	ctx, cancel := context.WithCancel(context.Background())
	dialer, err := water.NewDialerWithContext(ctx, &water.Config{TransportModuleBin: wasmPlain})
	if err != nil {
		t.Fatal(err)
	}
	first, err := dialer.DialContext(context.Background(), "tcp", echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := dialer.DialContext(context.Background(), "tcp", echo.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	cancel()
	time.Sleep(50 * time.Millisecond)
	roundtrip(t, second, []byte("survives constructor cancel"))
	third, err := dialer.DialContext(context.Background(), "tcp", echo.Addr().String())
	if err != nil {
		t.Fatalf("dial after constructor cancel: %v", err)
	}
	defer third.Close()
	roundtrip(t, third, []byte("dials after constructor cancel"))
}

// Every Accept after the first instantiates a new guest on the listener's shared
// runtime, and closing the listener must not tear down connections it accepted.
func TestListenerSharedAcceptsAndClose(t *testing.T) {
	config := &water.Config{TransportModuleBin: wasmPlain}
	lis, err := config.ListenContext(context.Background(), "tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()

	const n = 5
	peers := make([]net.Conn, n)
	accepted := make([]net.Conn, n)
	for i := range peers {
		if peers[i], err = net.Dial("tcp", lis.Addr().String()); err != nil {
			t.Fatal(err)
		}
		defer peers[i].Close()
		if accepted[i], err = lis.Accept(); err != nil {
			t.Fatal(err)
		}
		defer accepted[i].Close()
	}

	if err := lis.Close(); err != nil {
		t.Fatal(err)
	}
	forceGC()

	for i := range peers {
		msg := []byte{'m', byte('0' + i)}
		if _, err := peers[i].Write(msg); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len(msg))
		if _, err := io.ReadFull(accepted[i], buf); err != nil {
			t.Fatalf("conn %d after listener close: %v", i, err)
		}
		if !bytes.Equal(buf, msg) {
			t.Fatalf("conn %d: got %q, want %q", i, buf, msg)
		}
	}
}

func setupConfig(interpreter bool) *water.Config {
	c := &water.Config{
		TransportModuleBin:  wasmPlain,
		OverrideLogger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		ModuleConfigFactory: water.NewWazeroModuleConfigFactory(), // keeps guest stdout out of results
	}
	if interpreter {
		c.RuntimeConfig().Interpreter()
	} else {
		c.RuntimeConfig().Compiler()
	}
	return c
}

var engines = []struct {
	name        string
	interpreter bool
}{
	{"compiler", false},
	{"interpreter", true},
}

// BenchmarkConnSetupDialer measures dial + first byte + close on one long-lived
// Dialer, the steady-state cost of each new outbound connection.
func BenchmarkConnSetupDialer(b *testing.B) {
	for _, e := range engines {
		b.Run(e.name, func(b *testing.B) {
			echo := startEcho(b)
			defer echo.Close()
			dialer, err := water.NewDialerWithContext(context.Background(), setupConfig(e.interpreter))
			if err != nil {
				b.Fatal(err)
			}
			msg := []byte("x")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				conn, err := dialer.DialContext(context.Background(), "tcp", echo.Addr().String())
				if err != nil {
					b.Fatal(err)
				}
				roundtrip(b, conn, msg)
				conn.Close()
			}
		})
	}
}

// BenchmarkConnSetupListener measures accept + first byte + close on one
// long-lived Listener, the per-connection cost on the server side.
func BenchmarkConnSetupListener(b *testing.B) {
	for _, e := range engines {
		b.Run(e.name, func(b *testing.B) {
			config := setupConfig(e.interpreter)
			lis, err := config.ListenContext(context.Background(), "tcp", "localhost:0")
			if err != nil {
				b.Fatal(err)
			}
			defer lis.Close()
			msg := []byte("x")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				peer, err := net.Dial("tcp", lis.Addr().String())
				if err != nil {
					b.Fatal(err)
				}
				conn, err := lis.Accept()
				if err != nil {
					b.Fatal(err)
				}
				if _, err := peer.Write(msg); err != nil {
					b.Fatal(err)
				}
				buf := make([]byte, len(msg))
				if _, err := io.ReadFull(conn, buf); err != nil {
					b.Fatal(err)
				}
				conn.Close()
				peer.Close()
			}
		})
	}
}
