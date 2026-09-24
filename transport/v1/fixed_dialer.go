package v1

import (
	"context"
	"fmt"
	"runtime"
	"sync"

	"github.com/refraction-networking/water"
)

func init() {
	err := water.RegisterWATMFixedDialer("watm_dial_fixed_v1", NewFixedDialerWithContext)
	if err != nil {
		panic(err)
	}
}

type FixedDialer struct {
	config *water.Config
	ctx    context.Context
	shared *water.SharedRuntime

	prewarmedMu sync.Mutex
	prewarmed   water.Core

	water.UnimplementedFixedDialer // embedded to ensure forward compatibility
}

func NewFixedDialerWithContext(ctx context.Context, c *water.Config, core water.Core) (water.FixedDialer, error) {
	shared, core, err := adoptShared(ctx, c, core)
	if err != nil {
		return nil, err
	}
	f := &FixedDialer{
		config:    c.Clone(),
		ctx:       ctx,
		shared:    shared,
		prewarmed: core,
	}
	runtime.SetFinalizer(f, func(f *FixedDialer) { f.shared.Release() })
	return f, nil
}

func (f *FixedDialer) DialFixed() (conn water.Conn, err error) {
	return f.DialFixedContext(f.ctx)
}

func (f *FixedDialer) DialFixedContext(ctx context.Context) (conn water.Conn, err error) {
	if f.config == nil {
		return nil, fmt.Errorf("water: dialing with nil config is not allowed")
	}

	ctxReady, dialFixedReady := context.WithCancel(context.Background())
	go func() {
		defer dialFixedReady()
		var core water.Core

		f.prewarmedMu.Lock()
		if f.prewarmed != nil {
			core = f.prewarmed
			f.prewarmed = nil
			f.prewarmedMu.Unlock()
		} else {
			f.prewarmedMu.Unlock()
			core = f.shared.NewCore(ctx, f.config)
		}
		// See the matching KeepAlive in Dialer.DialContext.
		runtime.KeepAlive(f)

		conn, err = dialFixed(core)
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ctxReady.Done():
		return conn, err
	}
}
