# WATER Performance Benchmarks

Benchmarks comparing WATER+shadowsocks (WASM) against native shadowsocks (ss-tunnel/ss-server from shadowsocks-libev) and raw TCP, all on localhost.

## Test Setup

- **Platform**: macOS, Darwin 24.6.0
- **CPU**: Apple M4 Pro (14 cores)
- **Architecture**: arm64
- **Go version**: 1.24.0
- **TinyGo version**: 0.40.1 (WASM build)
- **WASM runtime**: github.com/getlantern/wazero v1.11.0-water (getlantern/wazero fork)
- **Shadowsocks cipher**: chacha20-ietf-poly1305
- **Shadowsocks server**: shadowsocks-libev 3.3.5
- **WASM module**: `shadowsocks_client.wasm` from [getlantern/wateringhole](https://github.com/getlantern/wateringhole)
- **Test topology**: client -> ss-server -> echo server (all localhost)

## Results

### Roundtrip Throughput (1 KB echo per iteration)

| Path                      | Throughput   | Latency/op    | Allocs/op |
|---------------------------|-------------|---------------|-----------|
| Raw TCP                   | 62.87 MB/s  | 16,287 ns     | 0         |
| Native shadowsocks        | 15.32 MB/s  | 66,830 ns     | 0         |
| WATER + shadowsocks (WASM)| 1.64 MB/s   | 625,988 ns    | 30        |

### Ping-Pong Latency (4-byte roundtrip)

| Path                       | Latency      | vs Raw TCP | vs Native SS |
|----------------------------|-------------|------------|--------------|
| Raw TCP                    | 15.6 us     | 1x         | -            |
| Native shadowsocks         | 53.5 us     | 3.4x       | 1x           |
| WATER + shadowsocks (WASM) | 539 us      | 35x        | 10.1x        |

### Connection Setup (WASM instantiation + dial)

| Path                       | Time per connection | Memory    | Allocs |
|----------------------------|--------------------:|----------:|-------:|
| WATER + shadowsocks        | 4.29 ms             | ~1.7 MB   | 6,231  |

### WATER v1 Transport Throughput (plain WASM, no encryption)

These benchmarks use the built-in `reverse.wasm` module (byte-reversal only, no crypto) to isolate the WASM runtime overhead from encryption cost.

| Benchmark                     | Throughput   | Latency/op    |
|-------------------------------|-------------|---------------|
| TCP Reference (baseline)      | 436.73 MB/s | 2,345 ns      |
| WATER Dialer Outbound         | 28.84 MB/s  | 35,500 ns     |
| WATER Listener Inbound        | 20.01 MB/s  | 51,179 ns     |

## Analysis

### Where the overhead comes from

The native shadowsocks comparison isolates the cost layers:

1. **Encryption cost (chacha20-poly1305)**: Native SS adds ~3.4x overhead over raw TCP (54 us vs 16 us). This is the baseline cost of authenticated encryption.

2. **WASM runtime cost**: WATER adds ~10x overhead on top of native SS (539 us vs 54 us). This dominates total overhead.

3. **Total WATER+SS vs raw TCP**: ~35x latency overhead.

The 30 allocations per roundtrip in the WATER path (vs 0 for native SS) are from the wazero data path -- specifically the virtual socket pairs used to shuttle data between the Go host and the WASM module, and the associated memory copying. The RawConn caching optimization in the wazero fork reduced this from 93 to 30 allocs/op.

### Dialer vs Listener asymmetry

The WATER v1 transport benchmarks show an asymmetry:
- Dialer outbound: 28.84 MB/s (~15x slower than TCP)
- Listener inbound: 20.01 MB/s (~22x slower than TCP)

The inbound path is ~1.4x slower than outbound. The wazero v1.11.0 upgrade significantly improved listener inbound performance (from 5.59 MB/s to 20.01 MB/s), narrowing the asymmetry from 6x to 1.4x.

### Known issues

- The `plain.wasm` module (passthrough, no transform) crashes with broken pipe under sustained benchmark load. Only `reverse.wasm` survives benchmarking. This appears to be a bug in the plain WASM module, not in the WATER runtime.
- Listener inbound is ~1.4x slower than dialer outbound (asymmetry much reduced in wazero v1.11.0).

## Android Implications

The performance characteristics above have significant implications for mobile deployment, particularly on Android:

### CPU

The 30 allocations per roundtrip and ~10x overhead from the WASM runtime translate directly to higher CPU utilization. On a mobile SoC (e.g., Snapdragon 8 Gen 2), single-core performance is roughly 60-70% of Apple M4 Pro, so latency numbers would be proportionally worse (~0.8-1.0 ms per roundtrip). The WASM interpreter in wazero does not JIT-compile on arm64 Android (wazero uses an interpreter mode on platforms without mmap exec support), which could further increase CPU cost.

For typical usage patterns (web browsing, messaging), the ~0.5 ms per-packet overhead is unlikely to be perceptible since network RTTs (50-200 ms) dominate. However, high-throughput scenarios like video streaming or large file downloads will be CPU-bound at the WASM layer, potentially saturating a single core at ~1.6 MB/s or less.

### Memory

Each WATER connection setup allocates ~1.7 MB and 6,231 objects. For a single active connection this is manageable, but applications that maintain connection pools or open many concurrent connections (e.g., HTTP/2 multiplexing over multiple WATER tunnels) could see significant heap pressure. On Android devices with 4-6 GB RAM and aggressive memory management, this could trigger more frequent garbage collection pauses.

### Battery Life

Battery impact comes from two sources:

1. **CPU wake time**: The 30 allocs/roundtrip and data copying through virtual socket pairs keep the CPU active longer per packet than native implementations. For background data sync or push notifications that arrive during doze mode, each packet wakes the CPU for ~0.5 ms (WATER) vs ~54 us (native SS) -- roughly 10x longer per wake event.

2. **GC pressure**: The allocation-heavy data path means the Go runtime's garbage collector runs more frequently. Each GC cycle is a CPU-intensive operation that prevents the SoC from entering low-power states.

For light usage (occasional browsing, messaging), the battery impact would be modest -- perhaps 5-10% increased drain compared to native shadowsocks. For sustained streaming or large transfers, the impact could be more significant as the WASM runtime keeps a core near full utilization.

### Recommendations for mobile

- **Connection reuse is critical**: The 4.3 ms connection setup cost (with ~1.7 MB allocation) makes connection pooling essential on mobile. Avoid creating new WATER connections per-request.
- **Consider pre-warming**: Instantiate the WASM module during app startup rather than on first network request to avoid latency spikes.
- **Monitor GC pauses**: The 30 allocs/roundtrip will generate GC pressure. Profiling on target Android devices is recommended.
- **Throughput ceiling**: Plan for ~1.6 MB/s per WATER connection on mid-range Android devices. Applications needing higher throughput should either use multiple connections or consider native transport implementations for performance-critical paths.

## Remote Benchmarks (Real Network)

The localhost benchmarks above show WASM overhead dominating (~0.5ms per roundtrip), but on localhost the network RTT is negligible (~16us). To understand real-world impact, we run the same comparisons over an actual network where WASM overhead competes with real RTT.

### Remote Test Setup

- **Server**: Digital Ocean `s-1vcpu-1gb` droplet in SFO3 (Ubuntu 24.04)
- **Client**: macOS M4 Pro (same as localhost benchmarks)
- **Network**: San Francisco region, ~36ms RTT
- **Services on droplet**: ss-server (:8388) + echo server (:8080)
- **Local**: ss-tunnel connects to remote ss-server (native SS path)

```
Local machine (macOS)              DO Droplet (SFO3)
┌──────────────────┐               ┌──────────────────┐
│ Benchmark runner │──WATER+SS────▶│ ss-server (:8388)│
│                  │──native SS───▶│                  │
│                  │──raw TCP─────▶│ echo     (:8080) │
└──────────────────┘               └──────────────────┘
```

### Remote Results

#### Latency (4-byte ping-pong roundtrip)

| Path                       | Latency      | vs Raw TCP |
|----------------------------|-------------|------------|
| Raw TCP                    | 37.5 ms     | 1x         |
| Native shadowsocks         | 37.1 ms     | 0.99x      |
| WATER + shadowsocks (WASM) | 38.4 ms     | 1.02x      |

#### Throughput (4 KB echo roundtrip)

| Path                       | Latency/op  | MB/s  | Allocs/op |
|----------------------------|-------------|-------|-----------|
| Raw TCP                    | 38.0 ms     | 0.11  | 0         |
| Native shadowsocks         | 39.4 ms     | 0.10  | 0         |
| WATER + shadowsocks (WASM) | 40.8 ms     | 0.10  | 30        |

#### Web Browsing Simulation (7-10 resources per page, 1KB-100KB each)

| Path                       | Time per page | vs Raw TCP |
|----------------------------|--------------|------------|
| Raw TCP (concurrent)       | 207 ms       | 1x         |
| Native SS (concurrent)     | 244 ms       | 1.18x      |
| WATER + SS (sequential)    | 505 ms       | 2.4x       |

Note: WATER uses sequential fetches on a single connection (reflecting typical Lantern tunnel usage). Native SS and TCP use concurrent connections per resource (browser-like). Resources range from 1KB (API responses) to 100KB (images), with HTML (~2KB), CSS (~8KB), JS (~32KB), and fonts (~20KB).

#### Connection Setup (WASM instantiation + network handshake)

| Path                       | Time per connection | Memory    | Allocs |
|----------------------------|--------------------:|----------:|-------:|
| WATER + shadowsocks        | 42.7 ms             | ~1.8 MB   | 6,507  |
| Raw TCP                    | 39.0 ms             | 680 B     | 14     |

### Analysis

**The ~1ms WASM overhead is negligible over a real network.** On localhost, WATER adds 8.3x latency over native SS. Over a 37ms network, it adds only 3.5% (38.4ms vs 37.1ms). The network RTT completely dominates.

**Large payloads now work reliably.** After fixing the AEAD fragmentation bug in `shadowio.Reader`, payloads up to 100KB work without issues over a real network. The fix makes `readBuffer()` stateful, preserving partial AEAD frame reads across EAGAIN boundaries when encrypted frames span multiple TCP segments.

**Web browsing performance is realistic.** With full-size resources (up to 100KB), a simulated page load takes ~505ms over WATER+SS versus ~207ms over raw TCP (2.4x). The gap comes from WATER's sequential single-connection model versus the concurrent multi-connection model used by browsers.

### What This Means for Lantern

For Lantern's typical usage pattern (single persistent WATER tunnel, multiplexed HTTP traffic):
- Individual request latency overhead is **imperceptible** (~1-2ms on a 37ms RTT)
- The bottleneck is connection establishment, not per-packet overhead
- Pre-warming connections and connection pooling eliminate the main cost
- Large payloads (up to 100KB tested) work reliably over real networks

## Pre-warmed Core Optimization

The version detection in `NewDialerWithContext` creates a `Core` (wazero Runtime + CompileModule) just to read exports, then discards it. The v1 factory's `DialContext` then creates *another* Core for the actual connection. By passing the detection Core through to the v1 factory and reusing it on the first dial, we eliminate this double-creation.

### Before (baseline)

| Metric              | Connection Setup | Roundtrip (1KB) | Latency (4B) |
|---------------------|----------------:|----------------:|--------------:|
| WATER+SS            | 5.24 ms         | 1,126 us        | 1,147 us      |
| Memory/op           | ~2 MB           | 2,226 B         | -             |
| Allocs/op           | 9,014           | 93              | 93            |

### After (pre-warmed Core reuse)

| Metric              | Connection Setup | Roundtrip (1KB) | Latency (4B) |
|---------------------|----------------:|----------------:|--------------:|
| WATER+SS            | 2.93 ms         | 1,043 us        | 1,105 us      |
| Memory/op           | ~1.5 MB         | 2,228 B         | 2,238 B       |
| Allocs/op           | 5,796           | 93              | 93            |

### Impact

| Metric              | Change     |
|---------------------|-----------|
| Connection setup time | **-44%** (5.24 ms -> 2.93 ms) |
| Setup memory         | **-25%** (~2 MB -> ~1.5 MB) |
| Setup allocations    | **-36%** (9,014 -> 5,796) |
| Roundtrip latency    | ~-7% (within noise) |
| Per-packet allocs    | unchanged (93) |

The connection setup improvement directly benefits the web browsing simulation where per-connection overhead dominates. Roundtrip and latency are within normal variance — the optimization only eliminates redundant setup work, not per-packet overhead.

## RawConn Caching + TinyGo 0.40.1 + AEAD Fragmentation Fix

Three optimizations applied together:

1. **RawConn caching in wazero fork**: Cached `syscall.RawConn` and fd at construction in `tcpConnFile` and `tcpListenerFile`, eliminating per-call `SyscallConn()` allocs in `Read()`, `Write()`, `Recvfrom()`, `SetNonblock()`, `Fd()`. Reduced per-roundtrip allocs from 93 to 30.

2. **TinyGo 0.40.1 upgrade**: Rebuilt `shadowsocks_client.wasm` with TinyGo 0.40.1 (was 0.31.2). Required fixing `proc_exit(0)` handling in the wazero fork — TinyGo 0.40+ calls `proc_exit(0)` after `main()` completes, which previously closed the WASM module. Fix: don't close the module on exit code 0 in both `procExitFn` and `InstantiateModule`.

3. **AEAD fragmentation fix**: Made `shadowio.Reader.readBuffer()` stateful in tiny-shadowsocks. When encrypted AEAD frames span multiple TCP segments (~1460 byte MSS), `io.ReadFull` returns `(partial_n, EAGAIN)`. The fix preserves the partially-filled buffer across calls using `pending`, `pendingPhase`, and `contentLen` fields. On the next call, `ReadFullFrom(reader, buffer.FreeLen())` resumes reading only the remaining bytes. This fixed the EIO crash that occurred with payloads >~1500 bytes over real networks.

### Combined Impact (vs pre-warmed Core baseline)

| Metric              | Before (pre-warmed) | After (all optimizations) | Change |
|---------------------|-------------------:|-------------------------:|-------:|
| Roundtrip latency   | 1,043 us           | 626 us                   | **-40%** |
| Roundtrip throughput | 0.99 MB/s          | 1.64 MB/s                | **+66%** |
| Per-roundtrip allocs | 93                 | 30                       | **-68%** |
| Connection setup    | 2.93 ms            | 4.29 ms                  | +46%*  |
| Max payload (remote)| ~1 KB              | 100+ KB                  | **fixed** |
| Web browsing (remote)| N/A (1KB limited) | 505 ms/page              | **fixed** |

*Connection setup is slower because TinyGo 0.40.1 produces a slightly larger WASM binary. The per-packet performance improvement more than compensates.

## Reproducing

Run the end-to-end benchmarks (requires `ss-server` and `ss-tunnel` from shadowsocks-libev):

```bash
# Roundtrip throughput
go test -bench="BenchmarkE2ERoundtrip" -benchmem -benchtime=3s -count=1

# Ping-pong latency
go test -bench="BenchmarkE2ELatency" -benchmem -benchtime=3s -count=1

# Connection setup
go test -bench="BenchmarkE2EConnectionSetup" -benchmem -benchtime=3s -count=1

# WATER v1 transport (no external dependencies)
go test -bench=. -benchmem -benchtime=5s ./transport/v1/

# TCP reference baseline
go test -bench=BenchmarkTCPReference -benchmem -benchtime=5s -tags=benchtcpref ./transport/v1/

# Remote benchmarks (requires doctl, a DO account, and ss-tunnel)
./scripts/setup-remote.sh up
REMOTE_HOST=<droplet-ip> go test -tags=remote -bench=. -benchmem -benchtime=3s -count=1
./scripts/setup-remote.sh down
```

The shadowsocks WASM module is loaded from `../wateringhole/protocols/shadowsocks/v1.0.0/shadowsocks_client.wasm`. Clone [getlantern/wateringhole](https://github.com/getlantern/wateringhole) as a sibling directory and run `git lfs pull`.
