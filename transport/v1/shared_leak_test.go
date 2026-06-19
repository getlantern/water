package v1_test

import (
	"context"
	"io"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/refraction-networking/water"
	v1 "github.com/refraction-networking/water/transport/v1"
)

// TestSharedDialer_NoLeak drives the shared-runtime dialer (compile once, reuse
// the runtime+module, fresh instance per dial) for many dials and asserts live
// heap stays flat — the Stage-2 fix for the per-dial core leak.
func TestSharedDialer_NoLeak(t *testing.T) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	g := ms.NumGC
	runtime.GC()
	runtime.ReadMemStats(&ms)
	if ms.NumGC-g == 0 {
		t.Skip("GC disabled")
	}

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { io.Copy(io.Discard, c); c.Close() }(c)
		}
	}()

	base := &water.Config{TransportModuleBin: wasmPlain}
	base.RuntimeConfig().Interpreter()
	base.RuntimeConfig().SetCloseOnContextDone(false)
	sd, err := v1.NewSharedDialer(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	defer sd.Close(context.Background())

	dialOnce := func() {
		cfg := &water.Config{TransportModuleBin: wasmPlain}
		conn, err := sd.DialContext(context.Background(), cfg, "tcp", lis.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}

	live := func() uint64 {
		for i := 0; i < 8; i++ {
			runtime.GC()
			runtime.Gosched()
			time.Sleep(20 * time.Millisecond)
		}
		runtime.ReadMemStats(&ms)
		return ms.HeapInuse
	}

	for i := 0; i < 20; i++ {
		dialOnce()
	}
	base0 := live()
	const N = 200
	for i := 0; i < N; i++ {
		dialOnce()
	}
	perDial := (float64(live()) - float64(base0)) / float64(N)
	t.Logf("shared dialer: %.1f KB/dial over %d dials", perDial/1024, N)
	if perDial > 50*1024 {
		t.Fatalf("shared dialer still leaks %.1f KB/dial", perDial/1024)
	}
}
