package water

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/refraction-networking/water/internal/wasip1"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// SharedRuntime hosts one wazero runtime, one compiled module, and a single
// shared "env" host module, all reused across many dials. Each dial instantiates
// only a fresh guest module instance on this runtime — avoiding the per-dial
// CompileModule that otherwise leaks the interpreter's compiled functions and
// burns CPU re-decoding the WASM binary on every connection.
//
// The shared env host functions hold no per-connection state: they dispatch to
// it via a registry keyed by the calling module instance, so many guest
// instances can run concurrently against one env without colliding.
type SharedRuntime struct {
	config  *Config
	runtime wazero.Runtime
	module  wazero.CompiledModule

	mu      sync.RWMutex
	reg     map[api.Module]*core
	closing bool // set once Close begins; blocks new registrations
	seq     atomic.Uint64
}

// NewCore mints a per-connection core that runs on this shared runtime. The
// runtime, compiled module, WASI, and env host module are reused; only a fresh
// guest instance is created per dial. config carries the per-dial
// TransportModuleConfig (e.g. remote_addr); the dial hooks are set separately
// via SetHostFuncs.
func (s *SharedRuntime) NewCore(ctx context.Context, config *Config) Core {
	c := &core{
		config:        config,
		shared:        s,
		runtime:       s.runtime,
		module:        s.module,
		importModules: make(map[string]wazero.HostModuleBuilder),
	}
	c.ctx, c.ctxCancel = context.WithCancel(ctx)
	c.instanceName = fmt.Sprintf("g%d", s.seq.Add(1))
	return c
}

// NewSharedRuntime builds the reusable runtime: WASI and the dispatching env are
// instantiated once, and the WASM binary is compiled once.
func NewSharedRuntime(ctx context.Context, config *Config) (*SharedRuntime, error) {
	s := &SharedRuntime{config: config, reg: make(map[api.Module]*core)}
	s.runtime = wazero.NewRuntimeWithConfig(ctx, config.RuntimeConfig().GetConfig())

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, s.runtime); err != nil {
		_ = s.runtime.Close(ctx)
		return nil, fmt.Errorf("water: WASI instantiate: %w", err)
	}
	if err := s.instantiateEnv(ctx); err != nil {
		_ = s.runtime.Close(ctx)
		return nil, fmt.Errorf("water: env instantiate: %w", err)
	}
	var err error
	if s.module, err = s.runtime.CompileModule(ctx, config.WATMBinOrPanic()); err != nil {
		_ = s.runtime.Close(ctx)
		return nil, fmt.Errorf("water: CompileModule: %w", err)
	}
	return s, nil
}

// Close releases the shared runtime. It first marks the runtime closing (so no
// new dial can register) and waits for in-flight instances to drain from the
// registry, because closing the runtime while a worker is still executing in the
// guest panics that worker; the per-connection close path stops each worker
// before freeing its instance, but a bulk runtime close would not. The wait is
// bounded by ctx and a grace period so a stuck instance can't block shutdown
// forever.
func (s *SharedRuntime) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	grace := time.NewTimer(2 * time.Second)
	defer grace.Stop()
	for {
		s.mu.RLock()
		n := len(s.reg)
		s.mu.RUnlock()
		if n == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return s.runtime.Close(ctx)
		case <-grace.C:
			return s.runtime.Close(ctx)
		case <-ticker.C:
		}
	}
	return s.runtime.Close(ctx)
}

// register records c under its instance so the shared env can dispatch to it. It
// returns false if the runtime is closing, in which case the caller must abandon
// the just-created instance rather than run it against a runtime about to close.
func (s *SharedRuntime) register(mod api.Module, c *core) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.reg[mod] = c
	return true
}

// clearHooks drops c's per-connection host hooks under the same lock the env
// dispatch path reads them, so an in-flight host call either sees a live hook or
// a nil one — never a hook into a half-closed connection.
func (s *SharedRuntime) clearHooks(c *core) {
	s.mu.Lock()
	c.hostDial, c.hostDialFixed, c.hostAccept = nil, nil, nil
	s.mu.Unlock()
}

func (s *SharedRuntime) unregister(mod api.Module) {
	s.mu.Lock()
	delete(s.reg, mod)
	s.mu.Unlock()
}

// hooks returns mod's per-connection host hooks snapshotted under the registry
// lock, so a concurrent close can't null a hook between the lookup and the call.
func (s *SharedRuntime) hooks(mod api.Module) (dial func(network, address string) int32, dialFixed, accept func() int32) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c := s.reg[mod]; c != nil {
		return c.hostDial, c.hostDialFixed, c.hostAccept
	}
	return nil, nil, nil
}

// instantiateEnv registers the one shared env host module. Each function looks
// up the calling instance's connection state and delegates to its per-dial host
// hooks (set by the transport's LinkNetworkInterface).
func (s *SharedRuntime) instantiateEnv(ctx context.Context) error {
	enodev := wasip1.EncodeWATERError(syscall.ENODEV)
	einval := wasip1.EncodeWATERError(syscall.EINVAL)

	waterDial := func(_ context.Context, mod api.Module, networkIovs, networkIovsLen, addressIovs, addressIovsLen int32) int32 {
		dial, _, _ := s.hooks(mod)
		if dial == nil {
			return enodev
		}
		networkBuf := make([]byte, 256)
		n, err := readIovs(mod, networkIovs, networkIovsLen, networkBuf)
		if err != nil {
			return einval
		}
		addressBuf := make([]byte, 256)
		m, err := readIovs(mod, addressIovs, addressIovsLen, addressBuf)
		if err != nil {
			return einval
		}
		return dial(string(networkBuf[:n]), string(addressBuf[:m]))
	}

	waterDialFixed := func(_ context.Context, mod api.Module) int32 {
		_, dialFixed, _ := s.hooks(mod)
		if dialFixed == nil {
			return enodev
		}
		return dialFixed()
	}

	waterAccept := func(_ context.Context, mod api.Module) int32 {
		_, _, accept := s.hooks(mod)
		if accept == nil {
			return enodev
		}
		return accept()
	}

	_, err := s.runtime.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithFunc(waterDial).Export("water_dial").
		NewFunctionBuilder().WithFunc(waterDialFixed).Export("water_dial_fixed").
		NewFunctionBuilder().WithFunc(waterAccept).Export("water_accept").
		Instantiate(ctx)
	return err
}

// readIovs reads the WASI iovec list at iovs/iovsLen from mod's memory into buf,
// mirroring (*core).ReadIovs but for an arbitrary calling module.
func readIovs(mod api.Module, iovs, iovsLen int32, buf []byte) (n int, err error) {
	mem := mod.Memory()
	iovsStop := uint32(iovsLen) << 3
	iovsBuf, ok := mem.Read(uint32(iovs), iovsStop)
	if !ok {
		return 0, errors.New("water: readIovs: failed to read iovs from memory")
	}
	for iovsPos := uint32(0); iovsPos < iovsStop; iovsPos += 8 {
		offset := le.Uint32(iovsBuf[iovsPos:])
		l := le.Uint32(iovsBuf[iovsPos+4:])
		b, ok := mem.Read(offset, l)
		if !ok {
			return 0, errors.New("water: readIovs: failed to read iov from memory")
		}
		nCopied := copy(buf[n:], b)
		n += nCopied
		if nCopied != len(b) {
			return n, errShortIovBuffer
		}
	}
	return n, nil
}

var errShortIovBuffer = errors.New("water: readIovs: short buffer")
