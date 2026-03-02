package v1

import (
	"context"
	"fmt"
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

	prewarmedMu sync.Mutex
	prewarmed   water.Core

	water.UnimplementedFixedDialer // embedded to ensure forward compatibility
}

func NewFixedDialerWithContext(ctx context.Context, c *water.Config, core water.Core) (water.FixedDialer, error) {
	return &FixedDialer{
		config:    c.Clone(),
		ctx:       ctx,
		prewarmed: core,
	}, nil
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
			core, err = water.NewCoreWithContext(ctx, f.config)
			if err != nil {
				return
			}
		}

		conn, err = dialFixed(core)
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ctxReady.Done():
		return conn, err
	}
}
