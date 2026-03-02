//go:build remote

package water_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/refraction-networking/water"
	_ "github.com/refraction-networking/water/transport/v1"
)

// Remote benchmarks compare WATER+SS, native SS, and raw TCP over a real
// network to a Digital Ocean droplet. The droplet runs ss-server (:8388) and
// an echo server (:8080).
//
// Run with:
//   REMOTE_HOST=<ip> go test -tags=remote -bench=. -benchmem -benchtime=3s -count=1

const (
	remoteSSPort   = "8388"
	remoteEchoPort = "8080"
)

func remoteHost(t testing.TB) string {
	t.Helper()
	h := os.Getenv("REMOTE_HOST")
	if h == "" {
		t.Skip("REMOTE_HOST not set")
	}
	return h
}

// remoteInfra holds addresses and the local ss-tunnel process for remote benchmarks.
type remoteInfra struct {
	host       string
	ssAddr     string // remote ss-server
	echoAddr   string // remote echo server
	tunnelAddr string // local ss-tunnel forwarding to remote echo via remote ss-server
	tunnelCmd  *exec.Cmd
}

func setupRemoteInfra(t testing.TB) *remoteInfra {
	t.Helper()
	host := remoteHost(t)
	inf := &remoteInfra{
		host:     host,
		ssAddr:   net.JoinHostPort(host, remoteSSPort),
		echoAddr: net.JoinHostPort(host, remoteEchoPort),
	}

	// Start local ss-tunnel that forwards to the remote echo server via remote ss-server
	localAddr := freePort(t)
	_, localPort, _ := net.SplitHostPort(localAddr)

	cmd := exec.Command("ss-tunnel",
		"-s", host,
		"-p", remoteSSPort,
		"-l", localPort,
		"-k", ssPassword,
		"-m", ssMethod,
		"-t", "300",
		"-L", net.JoinHostPort(host, remoteEchoPort),
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start ss-tunnel: %v", err)
	}
	inf.tunnelCmd = cmd

	waitForPort(t, localAddr)
	inf.tunnelAddr = localAddr
	return inf
}

func (inf *remoteInfra) cleanup() {
	if inf.tunnelCmd != nil {
		killCmd(inf.tunnelCmd)
	}
}

func remoteWaterDial(t testing.TB, inf *remoteInfra) net.Conn {
	t.Helper()
	config := &water.Config{
		TransportModuleBin:    wasmShadowsocks,
		TransportModuleConfig: water.TransportModuleConfigFromBytes(ssConfig(inf.host, remoteEchoPort)),
		ModuleConfigFactory:   water.NewWazeroModuleConfigFactory(),
	}
	dialer, err := water.NewDialerWithContext(context.Background(), config)
	if err != nil {
		t.Fatalf("failed to create WATER dialer: %v", err)
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", inf.ssAddr)
	if err != nil {
		t.Fatalf("WATER dial failed: %v", err)
	}
	return conn
}

// verifyEcho sends data and reads it back, confirming the path works.
func verifyEcho(t testing.TB, conn net.Conn, label string) {
	t.Helper()
	msg := make([]byte, 64)
	rand.Read(msg)
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("%s: write: %v", label, err)
	}
	buf := make([]byte, 64)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("%s: read: %v", label, err)
	}
	if !bytes.Equal(buf, msg) {
		t.Fatalf("%s: echo mismatch", label)
	}
	conn.SetDeadline(time.Time{})
}

// ---------- Benchmark: Latency (small ping-pong over real network) ----------

func BenchmarkRemoteLatency(b *testing.B) {
	if len(wasmShadowsocks) == 0 {
		b.Skip("shadowsocks wasm not available")
	}
	inf := setupRemoteInfra(b)
	defer inf.cleanup()

	benchLatency := func(b *testing.B, conn net.Conn) {
		msg := []byte("ping")
		buf := make([]byte, 4)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := conn.Write(msg); err != nil {
				b.Fatal(err)
			}
			if _, err := io.ReadFull(conn, buf); err != nil {
				b.Fatal(err)
			}
		}
	}

	b.Run("water+shadowsocks", func(b *testing.B) {
		conn := remoteWaterDial(b, inf)
		defer conn.Close()
		verifyEcho(b, conn, "water+ss")
		benchLatency(b, conn)
	})

	b.Run("native_shadowsocks", func(b *testing.B) {
		conn, err := net.Dial("tcp", inf.tunnelAddr)
		if err != nil {
			b.Fatal(err)
		}
		defer conn.Close()
		time.Sleep(100 * time.Millisecond) // let tunnel handshake settle
		verifyEcho(b, conn, "native_ss")
		benchLatency(b, conn)
	})

	b.Run("raw_tcp", func(b *testing.B) {
		conn, err := net.Dial("tcp", inf.echoAddr)
		if err != nil {
			b.Fatal(err)
		}
		defer conn.Close()
		verifyEcho(b, conn, "raw_tcp")
		benchLatency(b, conn)
	})
}

// ---------- Benchmark: Throughput (1 KB roundtrip, reports MB/s) ----------

func BenchmarkRemoteThroughput(b *testing.B) {
	if len(wasmShadowsocks) == 0 {
		b.Skip("shadowsocks wasm not available")
	}
	inf := setupRemoteInfra(b)
	defer inf.cleanup()

	const chunkSize = 32 * 1024 // 32 KB per roundtrip

	// Measure throughput via repeated echo roundtrips on a single connection.
	// The Go benchmark framework runs enough iterations to fill benchtime,
	// giving us MB/s over the real network.
	benchThroughput := func(b *testing.B, conn net.Conn) {
		msg := make([]byte, chunkSize)
		rand.Read(msg)
		buf := make([]byte, chunkSize)
		b.SetBytes(chunkSize)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := conn.Write(msg); err != nil {
				b.Fatalf("write: %v", err)
			}
			if _, err := io.ReadFull(conn, buf); err != nil {
				b.Fatalf("read: %v", err)
			}
		}
	}

	b.Run("water+shadowsocks", func(b *testing.B) {
		conn := remoteWaterDial(b, inf)
		defer conn.Close()
		verifyEcho(b, conn, "water+ss")
		benchThroughput(b, conn)
	})

	b.Run("native_shadowsocks", func(b *testing.B) {
		conn, err := net.Dial("tcp", inf.tunnelAddr)
		if err != nil {
			b.Fatal(err)
		}
		defer conn.Close()
		time.Sleep(100 * time.Millisecond)
		verifyEcho(b, conn, "native_ss")
		benchThroughput(b, conn)
	})

	b.Run("raw_tcp", func(b *testing.B) {
		conn, err := net.Dial("tcp", inf.echoAddr)
		if err != nil {
			b.Fatal(err)
		}
		defer conn.Close()
		verifyEcho(b, conn, "raw_tcp")
		benchThroughput(b, conn)
	})
}

// ---------- Benchmark: Web Browsing Simulation ----------

// simulatePageLoad models a "page load" as sequential resource fetches of
// varied sizes over a single connection. Each resource: write N bytes, read
// N bytes back. Sequential fetch on one connection reflects how Lantern
// typically tunnels — one WATER connection carrying multiplexed traffic.
//
// For the native SS and raw TCP paths, we open concurrent connections to
// show the best-case comparison.
func simulatePageLoadSingle(b *testing.B, conn net.Conn, resources []int) {
	b.Helper()
	for _, sz := range resources {
		msg := make([]byte, sz)
		rand.Read(msg)
		if _, err := conn.Write(msg); err != nil {
			b.Fatalf("write %d: %v", sz, err)
		}
		buf := make([]byte, sz)
		if _, err := io.ReadFull(conn, buf); err != nil {
			b.Fatalf("read %d: %v", sz, err)
		}
	}
}

func simulatePageLoadConcurrent(b *testing.B, dialFn func() (net.Conn, error), resources []int) {
	b.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, len(resources))

	for _, size := range resources {
		wg.Add(1)
		go func(sz int) {
			defer wg.Done()
			conn, err := dialFn()
			if err != nil {
				errs <- fmt.Errorf("dial: %w", err)
				return
			}
			defer conn.Close()

			conn.SetDeadline(time.Now().Add(30 * time.Second))
			msg := make([]byte, sz)
			rand.Read(msg)
			if _, err := conn.Write(msg); err != nil {
				errs <- fmt.Errorf("write %d: %w", sz, err)
				return
			}
			buf := make([]byte, sz)
			if _, err := io.ReadFull(conn, buf); err != nil {
				errs <- fmt.Errorf("read %d: %w", sz, err)
				return
			}
		}(size)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		b.Fatalf("page load error: %v", err)
	}
}

// randomPageResources generates a realistic set of resource sizes for one
// "page load": a mix of small, medium, and larger resources typical of a
// web page (HTML, CSS, JS, images).
func randomPageResources() []int {
	// Realistic resource sizes: HTML (~2KB), CSS (~8KB), JS (~30-60KB),
	// images (~10-100KB), fonts (~20KB), small API responses (~1KB).
	base := []int{
		2 * 1024,  // HTML
		8 * 1024,  // CSS
		1024,      // small API/JSON response
		32 * 1024, // JS bundle
		50 * 1024, // image
		20 * 1024, // font
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(4))
	extra := make([]int, int(n.Int64())+1)
	sizes := []int{4 * 1024, 16 * 1024, 64 * 1024, 100 * 1024}
	for i := range extra {
		idx, _ := rand.Int(rand.Reader, big.NewInt(int64(len(sizes))))
		extra[i] = sizes[idx.Int64()]
	}
	return append(base, extra...)
}

func BenchmarkRemoteWebBrowsing(b *testing.B) {
	if len(wasmShadowsocks) == 0 {
		b.Skip("shadowsocks wasm not available")
	}
	inf := setupRemoteInfra(b)
	defer inf.cleanup()

	pages := make([][]int, 20)
	for i := range pages {
		pages[i] = randomPageResources()
	}

	// WATER+SS: single connection, sequential resource fetches.
	// This reflects real Lantern usage (one tunnel, multiplexed).
	b.Run("water+shadowsocks", func(b *testing.B) {
		conn := remoteWaterDial(b, inf)
		defer conn.Close()
		verifyEcho(b, conn, "water+ss")
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			simulatePageLoadSingle(b, conn, pages[i%len(pages)])
		}
	})

	// Native SS and raw TCP: concurrent connections (best-case browser behavior)
	b.Run("native_shadowsocks", func(b *testing.B) {
		dialFn := func() (net.Conn, error) {
			return net.Dial("tcp", inf.tunnelAddr)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			simulatePageLoadConcurrent(b, dialFn, pages[i%len(pages)])
		}
	})

	b.Run("raw_tcp", func(b *testing.B) {
		dialFn := func() (net.Conn, error) {
			return net.Dial("tcp", inf.echoAddr)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			simulatePageLoadConcurrent(b, dialFn, pages[i%len(pages)])
		}
	})
}

// ---------- Benchmark: Connection Setup over real network ----------

func BenchmarkRemoteConnectionSetup(b *testing.B) {
	if len(wasmShadowsocks) == 0 {
		b.Skip("shadowsocks wasm not available")
	}
	inf := setupRemoteInfra(b)
	defer inf.cleanup()

	cfgBytes := ssConfig(inf.host, remoteEchoPort)

	b.Run("water+shadowsocks_dial", func(b *testing.B) {
		var conns []net.Conn
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			config := &water.Config{
				TransportModuleBin:    wasmShadowsocks,
				TransportModuleConfig: water.TransportModuleConfigFromBytes(cfgBytes),
				ModuleConfigFactory:   water.NewWazeroModuleConfigFactory(),
			}
			dialer, err := water.NewDialerWithContext(context.Background(), config)
			if err != nil {
				b.Fatal(err)
			}
			conn, err := dialer.DialContext(context.Background(), "tcp", inf.ssAddr)
			if err != nil {
				b.Fatal(err)
			}
			conns = append(conns, conn)
		}
		b.StopTimer()
		for _, c := range conns {
			c.Close()
		}
	})

	b.Run("raw_tcp_dial", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			conn, err := net.Dial("tcp", inf.echoAddr)
			if err != nil {
				b.Fatal(err)
			}
			conn.Close()
		}
	})
}
