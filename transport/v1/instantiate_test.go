package v1_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/refraction-networking/water"
)

// TestCoreInstantiateLeak isolates water's instantiate+close lifecycle from the
// dial/worker machinery: build a core, enable WASI, link stub host funcs,
// instantiate the module, then close. If live heap grows per iteration, the leak
// is in NewCoreWithContext/Instantiate/Close itself (raw wazero compile+close
// does not leak), not in the worker or networking.
func TestCoreInstantiateLeak(t *testing.T) {
	ctx := context.Background()
	var ms runtime.MemStats
	live := func() uint64 {
		for i := 0; i < 8; i++ {
			runtime.GC()
			runtime.Gosched()
			time.Sleep(20 * time.Millisecond)
		}
		runtime.ReadMemStats(&ms)
		return ms.HeapInuse
	}

	base := &water.Config{TransportModuleBin: wasmPlain}
	base.RuntimeConfig().Interpreter()
	base.RuntimeConfig().SetCloseOnContextDone(false) // match lanternd; no ctx watcher

	once := func() {
		core, err := water.NewCoreWithContext(ctx, base.Clone())
		if err != nil {
			t.Fatal(err)
		}
		if err := core.WASIPreview1(); err != nil {
			t.Fatal(err)
		}
		_ = core.ImportFunction("env", "water_dial", func(_, _, _, _ int32) int32 { return 0 })
		_ = core.ImportFunction("env", "water_dial_fixed", func() int32 { return 0 })
		_ = core.ImportFunction("env", "water_accept", func() int32 { return 0 })
		if err := core.Instantiate(); err != nil {
			t.Fatal(err)
		}
		// Exercise the interpreter callEngine by invoking an exported function,
		// without starting the long-running worker or any real network conn.
		if _, err := core.Invoke("watm_init_v1"); err != nil {
			t.Fatal(err)
		}
		core.Close()
	}

	for i := 0; i < 20; i++ {
		once()
	}
	b := live()
	const N = 200
	for i := 0; i < N; i++ {
		once()
	}
	perIter := (float64(live()) - float64(b)) / float64(N)
	t.Logf("core instantiate+close: %.1f KB/iter", perIter/1024)
	if perIter > 50*1024 {
		t.Fatalf("water core instantiate+close leaks %.1f KB/iter", perIter/1024)
	}
}
