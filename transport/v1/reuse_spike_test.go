package v1_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// TestReuseRuntimeNoLeak proves the Stage-2 model: ONE runtime, CompileModule
// ONCE, WASI ONCE, then per "dial" instantiate a fresh env host module + guest
// instance and close them. If live heap stays flat across N iterations, reusing
// the runtime+compiled module fixes the leak (vs ~5MB/dial when recompiling).
func TestReuseRuntimeNoLeak(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter().WithCloseOnContextDone(false))
	defer rt.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)
	cm, err := rt.CompileModule(ctx, wasmPlain)
	if err != nil {
		t.Fatal(err)
	}
	defer cm.Close(ctx)

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
		env, err := rt.NewHostModuleBuilder("env").
			NewFunctionBuilder().WithFunc(func(_, _, _, _ int32) int32 { return 0 }).Export("water_dial").
			NewFunctionBuilder().WithFunc(func() int32 { return 0 }).Export("water_dial_fixed").
			NewFunctionBuilder().WithFunc(func() int32 { return 0 }).Export("water_accept").
			Instantiate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		inst, err := rt.InstantiateModule(ctx, cm, wazero.NewModuleConfig().WithName(""))
		if err != nil {
			env.Close(ctx)
			t.Fatal(err)
		}
		if f := inst.ExportedFunction("watm_init_v1"); f != nil {
			f.Call(ctx)
		}
		inst.Close(ctx)
		env.Close(ctx)
	}

	for i := 0; i < 20; i++ {
		once()
	}
	base := live()
	const N = 200
	for i := 0; i < N; i++ {
		once()
	}
	perIter := (float64(live()) - float64(base)) / float64(N)
	t.Logf("reuse runtime+module: %.1f KB/iter", perIter/1024)
	if perIter > 50*1024 {
		t.Fatalf("reuse model still leaks %.1f KB/iter", perIter/1024)
	}
}
