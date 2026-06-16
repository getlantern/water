package v1

import (
	"context"

	"github.com/refraction-networking/water"
)

// SharedFixedDialer is a FixedDialer that compiles the WASM module once and
// reuses one runtime across all dials, instantiating only a fresh guest instance
// per connection. This avoids the per-dial CompileModule that leaks the
// interpreter's compiled functions and re-decodes the binary on every dial.
//
// Use it when the destination is supplied to the WATM via its per-dial config
// (watm_dial_fixed_v1), as with the shadowsocks WATM.
type SharedFixedDialer struct {
	shared *water.SharedRuntime
}

// NewSharedFixedDialer builds the shared runtime from baseConfig (its
// TransportModuleBin is compiled once; its RuntimeConfig selects the engine).
// Per-dial parameters (NetworkDialerFunc, TransportModuleConfig) are supplied to
// DialFixedContext, not here.
func NewSharedFixedDialer(ctx context.Context, baseConfig *water.Config) (*SharedFixedDialer, error) {
	shared, err := water.NewSharedRuntime(ctx, baseConfig)
	if err != nil {
		return nil, err
	}
	return &SharedFixedDialer{shared: shared}, nil
}

// DialFixedContext instantiates a fresh guest on the shared runtime and dials.
// config carries the per-dial NetworkDialerFunc (how to reach the server) and
// TransportModuleConfig (e.g. remote_addr, mounted at /conf/watm.cfg).
func (d *SharedFixedDialer) DialFixedContext(ctx context.Context, config *water.Config) (water.Conn, error) {
	core := d.shared.NewCore(ctx, config)
	conn, err := dialFixed(core)
	if err != nil {
		core.Close()
		return nil, err
	}
	return conn, nil
}

// Close releases the shared runtime and any instances still running on it.
func (d *SharedFixedDialer) Close(ctx context.Context) error {
	return d.shared.Close(ctx)
}

// SharedDialer is the address-passing (watm_dial_v1) counterpart of
// SharedFixedDialer: one compiled module reused across dials, a fresh guest
// instance per connection.
type SharedDialer struct {
	shared *water.SharedRuntime
}

// NewSharedDialer builds the shared runtime from baseConfig (compiled once).
func NewSharedDialer(ctx context.Context, baseConfig *water.Config) (*SharedDialer, error) {
	shared, err := water.NewSharedRuntime(ctx, baseConfig)
	if err != nil {
		return nil, err
	}
	return &SharedDialer{shared: shared}, nil
}

// DialContext instantiates a fresh guest on the shared runtime and dials the
// given address. config carries per-dial parameters (NetworkDialerFunc,
// TransportModuleConfig). Closing the returned conn tears down only its instance.
func (d *SharedDialer) DialContext(ctx context.Context, config *water.Config, network, address string) (water.Conn, error) {
	core := d.shared.NewCore(ctx, config)
	conn, err := dial(core, network, address)
	if err != nil {
		core.Close()
		return nil, err
	}
	return conn, nil
}

// Close releases the shared runtime.
func (d *SharedDialer) Close(ctx context.Context) error {
	return d.shared.Close(ctx)
}
