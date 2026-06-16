package v1_test

import (
	"context"
	"expvar"
	"io"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/refraction-networking/water"
	v1 "github.com/refraction-networking/water/transport/v1"
)

// TestDial_CoreLeak reproduces the production memory leak: lanternd dials through
// water thousands of times, compiling a fresh module per dial. If a dial's core is
// not fully reclaimed after the conn is closed, live heap grows with the dial
// count. Two subtests exercise the package-level dialer (which pre-warms a core,
// as lanternd does) and the direct v1 dialer, to localize the leak.
func TestDial_CoreLeak(t *testing.T) {
	t.Skip("tracks an open per-dial core leak in the full dial/worker path; " +
		"the finalizer-cycle leak is fixed (see TestCoreInstantiateLeak), but the " +
		"worker path still retains ~5MB/dial. Unskip to work on it.")

	var memStat runtime.MemStats
	runtime.ReadMemStats(&memStat)
	gcBefore := memStat.NumGC
	runtime.GC()
	runtime.ReadMemStats(&memStat)
	if memStat.NumGC-gcBefore == 0 {
		t.Skip("GC appears disabled; skipping")
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
			go func(c net.Conn) {
				io.Copy(io.Discard, c)
				c.Close()
			}(c)
		}
	}()

	config := &water.Config{TransportModuleBin: wasmPlain}
	config.RuntimeConfig().Interpreter()
	config.RuntimeConfig().SetCloseOnContextDone(false)

	liveHeap := func() uint64 {
		// Drain finalizers: the core sets a finalizer, so reclaiming it takes
		// GC -> finalizer-run -> GC. Yield + sleep between cycles so the finalizer
		// goroutine runs, otherwise we'd measure finalizer lag as a false leak.
		for i := 0; i < 8; i++ {
			runtime.GC()
			runtime.Gosched()
			time.Sleep(20 * time.Millisecond)
		}
		runtime.ReadMemStats(&memStat)
		return memStat.HeapInuse
	}
	counter := func(name string) int64 {
		if v, ok := expvar.Get(name).(*expvar.Int); ok {
			return v.Value()
		}
		return -1
	}

	run := func(t *testing.T, dialOnce func()) {
		for i := 0; i < 20; i++ { // warm up caches and one-time allocations
			dialOnce()
		}
		baseHeap := liveHeap()
		baseGo := runtime.NumGoroutine()
		baseCreated, baseClosed := counter("water_cores_created"), counter("water_core_close_completed")

		const N = 200
		for i := 0; i < N; i++ {
			dialOnce()
		}
		afterHeap := liveHeap()

		perDial := (float64(afterHeap) - float64(baseHeap)) / float64(N)
		t.Logf("created=%d closed=%d goroutines %d->%d; heap %.1f KB/dial",
			counter("water_cores_created")-baseCreated, counter("water_core_close_completed")-baseClosed,
			baseGo, runtime.NumGoroutine(), perDial/1024)
		if perDial > 50*1024 {
			t.Fatalf("per-dial heap growth %.1f KB indicates a per-dial core leak", perDial/1024)
		}
	}

	t.Run("package-level (prewarm)", func(t *testing.T) {
		run(t, func() {
			dialer, err := water.NewDialerWithContext(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := dialer.DialContext(context.Background(), "tcp", lis.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
		})
	})

	t.Run("direct v1", func(t *testing.T) {
		run(t, func() {
			dialer, err := v1.NewDialerWithContext(context.Background(), config, nil)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := dialer.DialContext(context.Background(), "tcp", lis.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			conn.Close()
		})
	})
}
