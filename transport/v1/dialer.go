package v1

import (
	"context"
	"fmt"
	"runtime"
	"sync"

	"github.com/refraction-networking/water"
)

func init() {
	err := water.RegisterWATMDialer("watm_dial_v1", NewDialerWithContext)
	if err != nil {
		panic(err)
	}
}

// Dialer implements [water.Dialer] utilizing Water WATM API v1.
type Dialer struct {
	config *water.Config
	ctx    context.Context
	shared *water.SharedRuntime

	prewarmedMu sync.Mutex
	prewarmed   water.Core // set at creation, consumed by first DialContext

	water.UnimplementedDialer // embedded to ensure forward compatibility
}

// NewDialer creates a new [water.Dialer] from the given [water.Config].
//
// Deprecated: use [NewDialerWithContext] instead.
func NewDialer(c *water.Config) (water.Dialer, error) {
	return NewDialerWithContext(context.Background(), c, nil)
}

// NewDialerWithContext creates a new [water.Dialer] from the given [water.Config]
// with the given [context.Context].
//
// The context is used as the default context for call to [Dialer.Dial].
// If a non-nil Core is provided, it will be used for the first DialContext call,
// avoiding the cost of creating a new Core (runtime + module compilation).
//
// Every dial runs as a fresh guest instance on one runtime and compiled module
// shared by the Dialer. Dialer has no Close, so the runtime is released when
// the Dialer is garbage collected and closes once its last connection does.
func NewDialerWithContext(ctx context.Context, c *water.Config, core water.Core) (water.Dialer, error) {
	shared, core, err := adoptShared(ctx, c, core)
	if err != nil {
		return nil, err
	}
	d := &Dialer{
		config:    c.Clone(),
		ctx:       ctx,
		shared:    shared,
		prewarmed: core,
	}
	runtime.SetFinalizer(d, func(d *Dialer) { d.shared.Release() })
	return d, nil
}

// Dial dials the network address using the dialerFunc specified in config.
//
// Implements [water.Dialer].
func (d *Dialer) Dial(network, address string) (conn water.Conn, err error) {
	return d.DialContext(d.ctx, network, address)
}

// DialContext dials the network address using the dialerFunc specified in config.
//
// The context is passed to [water.NewCoreWithContext] to control the lifetime of
// the call to function calls into the WebAssembly module.
// If the context is canceled or reaches its deadline, any current and future
// function call will return with an error.
// Call [water.WazeroRuntimeConfigFactory.SetCloseOnContextDone] with false to
// disable this behavior.
//
// Implements [water.Dialer].
func (d *Dialer) DialContext(ctx context.Context, network, address string) (conn water.Conn, err error) {
	if d.config == nil {
		return nil, fmt.Errorf("water: dialing with nil config is not allowed")
	}

	ctxReady, dialReady := context.WithCancel(context.Background())
	go func() {
		defer dialReady()
		var core water.Core

		// Use pre-warmed core for first dial to avoid redundant
		// runtime creation and module compilation.
		d.prewarmedMu.Lock()
		if d.prewarmed != nil {
			core = d.prewarmed
			d.prewarmed = nil
			d.prewarmedMu.Unlock()
		} else {
			d.prewarmedMu.Unlock()
			core = d.shared.NewCore(ctx, d.config)
		}

		conn, err = dial(core, network, address)
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ctxReady.Done():
		return conn, err
	}
}
