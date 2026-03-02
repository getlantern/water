package water_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/refraction-networking/water"
	_ "github.com/refraction-networking/water/transport/v1"
)

const (
	ssPassword = "8JCsPssfgS8tiRwiMlhARg=="
	ssMethod   = "chacha20-ietf-poly1305"
)

var wasmShadowsocks []byte

func init() {
	var err error
	wasmShadowsocks, err = os.ReadFile("../wateringhole/protocols/shadowsocks/v1.0.0/shadowsocks_client.wasm")
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: could not load shadowsocks wasm: %v\n", err)
	}
}

// startEchoServer starts a TCP server that echoes back everything it receives.
func startEchoServer(t *testing.T) (net.Listener, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start echo server: %v", err)
	}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()
	return lis, lis.Addr().String()
}

func freePort(t testing.TB) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	addr := lis.Addr().String()
	lis.Close()
	return addr
}

func waitForPort(t testing.TB, addr string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", addr)
}

// startSSServer starts a shadowsocks-libev server and returns the listening address.
func startSSServer(t testing.TB) (*exec.Cmd, string) {
	t.Helper()

	ssAddr := freePort(t)
	_, ssPort, _ := net.SplitHostPort(ssAddr)

	cmd := exec.Command("ss-server",
		"-s", "127.0.0.1",
		"-p", ssPort,
		"-k", ssPassword,
		"-m", ssMethod,
		"-t", "300",
		"-u",
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start ss-server: %v", err)
	}

	waitForPort(t, ssAddr)
	return cmd, ssAddr
}

// startSSTunnel starts ss-tunnel (native shadowsocks client) as a local TCP
// forwarder: local_port -> ss-server -> destAddr. Returns the cmd and the
// local address to connect to.
func startSSTunnel(t testing.TB, ssServerAddr, destAddr string) (*exec.Cmd, string) {
	t.Helper()

	localAddr := freePort(t)
	_, localPort, _ := net.SplitHostPort(localAddr)
	_, ssPort, _ := net.SplitHostPort(ssServerAddr)

	cmd := exec.Command("ss-tunnel",
		"-s", "127.0.0.1",
		"-p", ssPort,
		"-l", localPort,
		"-k", ssPassword,
		"-m", ssMethod,
		"-t", "300",
		"-L", destAddr,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start ss-tunnel: %v", err)
	}

	waitForPort(t, localAddr)
	return cmd, localAddr
}

func ssConfig(destAddr, destPort string) []byte {
	cfg := map[string]any{
		"remote_addr": destAddr,
		"remote_port": destPort,
		"password":    ssPassword,
		"method":      ssMethod,
	}
	b, _ := json.Marshal(cfg)
	return b
}

func killCmd(cmd *exec.Cmd) {
	cmd.Process.Kill()
	cmd.Wait()
}

func TestE2EShadowsocks(t *testing.T) {
	if len(wasmShadowsocks) == 0 {
		t.Skip("shadowsocks wasm not available")
	}

	// 1. Start echo server (the ultimate destination)
	echoLis, echoAddr := startEchoServer(t)
	defer echoLis.Close()
	echoHost, echoPort, _ := net.SplitHostPort(echoAddr)
	t.Logf("Echo server: %s", echoAddr)

	// 2. Start shadowsocks server
	ssCmd, ssAddr := startSSServer(t)
	defer killCmd(ssCmd)
	t.Logf("SS server: %s", ssAddr)

	// 3. Configure WATER with shadowsocks WASM
	config := &water.Config{
		TransportModuleBin:    wasmShadowsocks,
		TransportModuleConfig: water.TransportModuleConfigFromBytes(ssConfig(echoHost, echoPort)),
		ModuleConfigFactory:   water.NewWazeroModuleConfigFactory(),
	}

	// 4. Create WATER dialer
	dialer, err := water.NewDialerWithContext(context.Background(), config)
	if err != nil {
		t.Fatalf("failed to create WATER dialer: %v", err)
	}

	// 5. Dial through WATER -> ss-server -> echo server
	conn, err := dialer.DialContext(context.Background(), "tcp", ssAddr)
	if err != nil {
		t.Fatalf("failed to dial: %v", err)
	}
	defer conn.Close()

	// 6. Test bidirectional communication
	t.Run("echo_roundtrip", func(t *testing.T) {
		for i := 0; i < 10; i++ {
			msg := make([]byte, 1024)
			rand.Read(msg)

			_, err := conn.Write(msg)
			if err != nil {
				t.Fatalf("write error on iteration %d: %v", i, err)
			}

			buf := make([]byte, 1024)
			n, err := io.ReadFull(conn, buf)
			if err != nil {
				t.Fatalf("read error on iteration %d: %v", i, err)
			}
			if n != len(msg) {
				t.Fatalf("short read: got %d, want %d", n, len(msg))
			}
			if !bytes.Equal(buf[:n], msg) {
				t.Fatal("echo mismatch")
			}
		}
	})

	t.Run("large_payload", func(t *testing.T) {
		msg := make([]byte, 64*1024)
		rand.Read(msg)

		_, err := conn.Write(msg)
		if err != nil {
			t.Fatalf("write error: %v", err)
		}

		buf := make([]byte, len(msg))
		_, err = io.ReadFull(conn, buf)
		if err != nil {
			t.Fatalf("read error: %v", err)
		}
		if !bytes.Equal(buf, msg) {
			t.Fatal("echo mismatch for large payload")
		}
	})
}

// setupSSInfra creates the shared infrastructure: echo server, ss-server, and
// ss-tunnel (native SS client). Returns cleanup func plus addresses.
type ssInfra struct {
	echoAddr    string
	echoHost    string
	echoPort    string
	ssAddr      string
	tunnelAddr  string
	cleanupOnce sync.Once
	cleanups    []func()
}

func (s *ssInfra) cleanup() {
	s.cleanupOnce.Do(func() {
		for i := len(s.cleanups) - 1; i >= 0; i-- {
			s.cleanups[i]()
		}
	})
}

func setupSSInfra(t testing.TB) *ssInfra {
	t.Helper()
	inf := &ssInfra{}

	// Echo server
	echoLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	inf.cleanups = append(inf.cleanups, func() { echoLis.Close() })
	inf.echoAddr = echoLis.Addr().String()
	inf.echoHost, inf.echoPort, _ = net.SplitHostPort(inf.echoAddr)

	go func() {
		for {
			conn, err := echoLis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()

	// ss-server
	ssCmd, ssAddr := startSSServer(t)
	inf.cleanups = append(inf.cleanups, func() { killCmd(ssCmd) })
	inf.ssAddr = ssAddr

	// ss-tunnel (native SS client forwarding to echo server)
	tunnelCmd, tunnelAddr := startSSTunnel(t, ssAddr, inf.echoAddr)
	inf.cleanups = append(inf.cleanups, func() { killCmd(tunnelCmd) })
	inf.tunnelAddr = tunnelAddr

	return inf
}

func waterDialSS(b testing.TB, inf *ssInfra) net.Conn {
	b.Helper()
	config := &water.Config{
		TransportModuleBin:    wasmShadowsocks,
		TransportModuleConfig: water.TransportModuleConfigFromBytes(ssConfig(inf.echoHost, inf.echoPort)),
		ModuleConfigFactory:   water.NewWazeroModuleConfigFactory(),
	}
	dialer, err := water.NewDialerWithContext(context.Background(), config)
	if err != nil {
		b.Fatalf("failed to create WATER dialer: %v", err)
	}
	conn, err := dialer.DialContext(context.Background(), "tcp", inf.ssAddr)
	if err != nil {
		b.Fatalf("failed to dial: %v", err)
	}
	return conn
}

func BenchmarkE2ERoundtrip(b *testing.B) {
	if len(wasmShadowsocks) == 0 {
		b.Skip("shadowsocks wasm not available")
	}

	inf := setupSSInfra(b)
	defer inf.cleanup()

	benchRoundtrip := func(b *testing.B, conn net.Conn) {
		msg := make([]byte, 1024)
		recvBuf := make([]byte, 1024)
		b.SetBytes(1024)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rand.Read(msg)
			if _, err := conn.Write(msg); err != nil {
				b.Fatalf("write error at iteration %d: %v", i, err)
			}
			if _, err := io.ReadFull(conn, recvBuf); err != nil {
				b.Fatalf("read error at iteration %d: %v", i, err)
			}
		}
		b.StopTimer()
	}

	b.Run("water+shadowsocks", func(b *testing.B) {
		conn := waterDialSS(b, inf)
		defer conn.Close()
		benchRoundtrip(b, conn)
	})

	b.Run("native_shadowsocks", func(b *testing.B) {
		conn, err := net.Dial("tcp", inf.tunnelAddr)
		if err != nil {
			b.Fatal(err)
		}
		defer conn.Close()
		time.Sleep(50 * time.Millisecond)
		benchRoundtrip(b, conn)
	})

	b.Run("raw_tcp", func(b *testing.B) {
		conn, err := net.Dial("tcp", inf.echoAddr)
		if err != nil {
			b.Fatal(err)
		}
		defer conn.Close()
		benchRoundtrip(b, conn)
	})
}

func BenchmarkE2ELatency(b *testing.B) {
	if len(wasmShadowsocks) == 0 {
		b.Skip("shadowsocks wasm not available")
	}

	inf := setupSSInfra(b)
	defer inf.cleanup()

	benchLatency := func(b *testing.B, conn net.Conn) {
		msg := []byte("ping")
		recvBuf := make([]byte, 4)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := conn.Write(msg); err != nil {
				b.Fatal(err)
			}
			if _, err := io.ReadFull(conn, recvBuf); err != nil {
				b.Fatal(err)
			}
		}
	}

	b.Run("water+shadowsocks", func(b *testing.B) {
		conn := waterDialSS(b, inf)
		defer conn.Close()
		benchLatency(b, conn)
	})

	b.Run("native_shadowsocks", func(b *testing.B) {
		conn, err := net.Dial("tcp", inf.tunnelAddr)
		if err != nil {
			b.Fatal(err)
		}
		defer conn.Close()
		time.Sleep(50 * time.Millisecond)
		benchLatency(b, conn)
	})

	b.Run("raw_tcp", func(b *testing.B) {
		conn, err := net.Dial("tcp", inf.echoAddr)
		if err != nil {
			b.Fatal(err)
		}
		defer conn.Close()
		benchLatency(b, conn)
	})
}

func BenchmarkE2EConnectionSetup(b *testing.B) {
	if len(wasmShadowsocks) == 0 {
		b.Skip("shadowsocks wasm not available")
	}

	inf := setupSSInfra(b)
	defer inf.cleanup()

	cfgBytes := ssConfig(inf.echoHost, inf.echoPort)

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
