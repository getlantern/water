package water

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"

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

	mu  sync.RWMutex
	reg map[api.Module]*core
	seq atomic.Uint64
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

// Close releases the shared runtime and every instance still running on it.
func (s *SharedRuntime) Close(ctx context.Context) error {
	return s.runtime.Close(ctx)
}

func (s *SharedRuntime) register(mod api.Module, c *core) {
	s.mu.Lock()
	s.reg[mod] = c
	s.mu.Unlock()
}

func (s *SharedRuntime) unregister(mod api.Module) {
	s.mu.Lock()
	delete(s.reg, mod)
	s.mu.Unlock()
}

func (s *SharedRuntime) lookup(mod api.Module) *core {
	s.mu.RLock()
	c := s.reg[mod]
	s.mu.RUnlock()
	return c
}

// instantiateEnv registers the one shared env host module. Each function looks
// up the calling instance's connection state and delegates to its per-dial host
// hooks (set by the transport's LinkNetworkInterface).
func (s *SharedRuntime) instantiateEnv(ctx context.Context) error {
	enodev := wasip1.EncodeWATERError(syscall.ENODEV)
	einval := wasip1.EncodeWATERError(syscall.EINVAL)

	waterDial := func(_ context.Context, mod api.Module, networkIovs, networkIovsLen, addressIovs, addressIovsLen int32) int32 {
		c := s.lookup(mod)
		if c == nil || c.hostDial == nil {
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
		return c.hostDial(string(networkBuf[:n]), string(addressBuf[:m]))
	}

	waterDialFixed := func(_ context.Context, mod api.Module) int32 {
		c := s.lookup(mod)
		if c == nil || c.hostDialFixed == nil {
			return enodev
		}
		return c.hostDialFixed()
	}

	waterAccept := func(_ context.Context, mod api.Module) int32 {
		c := s.lookup(mod)
		if c == nil || c.hostAccept == nil {
			return enodev
		}
		return c.hostAccept()
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
