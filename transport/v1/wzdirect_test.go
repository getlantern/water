package v1_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
)

// TestWazeroCompileLeak isolates wazero from water: repeatedly CompileModule on a
// fresh interpreter runtime (no cache) and Close it. If live heap grows per
// iteration, the leak is in wazero's CompileModule/Close, not water's lifecycle.
func TestWazeroCompileLeak(t *testing.T) {
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
	once := func() {
		r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
		cm, err := r.CompileModule(ctx, wasmPlain)
		if err != nil {
			t.Fatal(err)
		}
		_ = cm.Close(ctx)
		_ = r.Close(ctx)
	}
	for i := 0; i < 20; i++ {
		once()
	}
	base := live()
	const N = 200
	for i := 0; i < N; i++ {
		once()
	}
	perDial := (float64(live()) - float64(base)) / float64(N)
	t.Logf("wazero compile+close: %.1f KB/iter", perDial/1024)
	if perDial > 50*1024 {
		t.Fatalf("wazero leaks %.1f KB per compile+close", perDial/1024)
	}
}
