package v1_test

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// TestReuseSharedEnv proves the concurrent Stage-2 model WITHOUT a resolver: ONE
// runtime, compiled ONCE, ONE shared env host module (its funcs dispatch per
// caller via the api.Module arg), and MANY concurrently-live guest instances with
// unique names. Validates no name collision + no per-dial leak.
func TestReuseSharedEnv(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter().WithCloseOnContextDone(false))
	defer rt.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, rt)

	// One shared env; funcs receive the calling module (api.Module) so the real
	// impl can look up per-connection state from a registry keyed by the caller.
	_, err := rt.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithFunc(func(_ context.Context, _ api.Module, _, _, _, _ int32) int32 { return 0 }).Export("water_dial").
		NewFunctionBuilder().WithFunc(func(_ context.Context, _ api.Module) int32 { return 0 }).Export("water_dial_fixed").
		NewFunctionBuilder().WithFunc(func(_ context.Context, _ api.Module) int32 { return 0 }).Export("water_accept").
		Instantiate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cm, err := rt.CompileModule(ctx, wasmPlain)
	if err != nil {
		t.Fatal(err)
	}
	defer cm.Close(ctx)

	var seq int
	openGuest := func() func() {
		seq++
		inst, err := rt.InstantiateModule(ctx, cm, wazero.NewModuleConfig().WithName(fmt.Sprintf("g-%d", seq)))
		if err != nil {
			t.Fatalf("instantiate g-%d: %v", seq, err)
		}
		if f := inst.ExportedFunction("watm_init_v1"); f != nil {
			f.Call(ctx)
		}
		return func() { inst.Close(ctx) }
	}

	var ms runtime.MemStats
	live := func() uint64 {
		for i := 0; i < 8; i++ {
			runtime.GC(); runtime.Gosched(); time.Sleep(20 * time.Millisecond)
		}
		runtime.ReadMemStats(&ms)
		return ms.HeapInuse
	}
	const concurrent = 8
	round := func() {
		cs := make([]func(), concurrent)
		for i := range cs {
			cs[i] = openGuest()
		}
		for _, c := range cs {
			c()
		}
	}
	for i := 0; i < 10; i++ {
		round()
	}
	base := live()
	const rounds = 25
	for i := 0; i < rounds; i++ {
		round()
	}
	perDial := (float64(live()) - float64(base)) / float64(rounds*concurrent)
	t.Logf("shared-env concurrent reuse: %.1f KB/dial over %d dials", perDial/1024, rounds*concurrent)
	if perDial > 50*1024 {
		t.Fatalf("shared-env model leaks %.1f KB/dial", perDial/1024)
	}
}
