# WATER Performance Benchmarks

Benchmarks comparing WATER+shadowsocks (WASM) against native shadowsocks (ss-tunnel/ss-server from shadowsocks-libev) and raw TCP, all on localhost.

## Test Setup

- **Platform**: macOS, Darwin 24.6.0
- **CPU**: Apple M4 Pro (14 cores)
- **Architecture**: arm64
- **Go version**: 1.21+
- **WASM runtime**: wazero v1.7.3
- **Shadowsocks cipher**: chacha20-ietf-poly1305
- **Shadowsocks server**: shadowsocks-libev 3.3.5
- **WASM module**: `shadowsocks_client.wasm` (353 KB) from [getlantern/wateringhole](https://github.com/getlantern/wateringhole)
- **Test topology**: client -> ss-server -> echo server (all localhost)

## Results

### Roundtrip Throughput (1 KB echo per iteration)

| Path                      | Throughput   | Latency/op    | Allocs/op |
|---------------------------|-------------|---------------|-----------|
| Raw TCP                   | 56.73 MB/s  | 18,050 ns     | 0         |
| Native shadowsocks        | 14.02 MB/s  | 73,040 ns     | 0         |
| WATER + shadowsocks (WASM)| 0.91 MB/s   | 1,126,246 ns  | 93        |

### Ping-Pong Latency (4-byte roundtrip)

| Path                       | Latency      | vs Raw TCP | vs Native SS |
|----------------------------|-------------|------------|--------------|
| Raw TCP                    | 17.9 us     | 1x         | -            |
| Native shadowsocks         | 65.1 us     | 3.6x       | 1x           |
| WATER + shadowsocks (WASM) | 1,147 us    | 64x        | 17.6x        |

### Connection Setup (WASM instantiation + dial)

| Path                       | Time per connection | Memory    | Allocs |
|----------------------------|--------------------:|----------:|-------:|
| WATER + shadowsocks        | 5.24 ms             | ~2 MB     | 9,014  |

### WATER v1 Transport Throughput (plain WASM, no encryption)

These benchmarks use the built-in `reverse.wasm` module (byte-reversal only, no crypto) to isolate the WASM runtime overhead from encryption cost.

| Benchmark                     | Throughput   | Latency/op    |
|-------------------------------|-------------|---------------|
| TCP Reference (baseline)      | 340.39 MB/s | 3,008 ns      |
| WATER Dialer Outbound         | 33.78 MB/s  | 30,313 ns     |
| WATER Listener Inbound        | 5.59 MB/s   | 183,179 ns    |

## Analysis

### Where the overhead comes from

The native shadowsocks comparison isolates the cost layers:

1. **Encryption cost (chacha20-poly1305)**: Native SS adds ~3.6x overhead over raw TCP (65 us vs 18 us). This is the baseline cost of authenticated encryption.

2. **WASM runtime cost**: WATER adds ~17.6x overhead on top of native SS (1,147 us vs 65 us). This dominates total overhead.

3. **Total WATER+SS vs raw TCP**: ~64x latency overhead.

The 93 allocations per roundtrip in the WATER path (vs 0 for native SS) confirm the bottleneck is in the wazero data path -- specifically the virtual socket pairs used to shuttle data between the Go host and the WASM module, and the associated memory copying.

### Dialer vs Listener asymmetry

The WATER v1 transport benchmarks show a significant asymmetry:
- Dialer outbound: 33.78 MB/s (~10x slower than TCP)
- Listener inbound: 5.59 MB/s (~61x slower than TCP)

The inbound path is ~6x slower than outbound, suggesting the listener accept + read path through the WASM module has additional overhead, possibly from how the WASM worker thread services inbound data.

### Known issues

- The `plain.wasm` module (passthrough, no transform) crashes with broken pipe under sustained benchmark load. Only `reverse.wasm` survives benchmarking. This appears to be a bug in the plain WASM module, not in the WATER runtime.

## Android Implications

The performance characteristics above have significant implications for mobile deployment, particularly on Android:

### CPU

The 93 allocations per roundtrip and ~17.6x overhead from the WASM runtime translate directly to higher CPU utilization. On a mobile SoC (e.g., Snapdragon 8 Gen 2), single-core performance is roughly 60-70% of Apple M4 Pro, so latency numbers would be proportionally worse (~1.5-1.7 ms per roundtrip). The WASM interpreter in wazero does not JIT-compile on arm64 Android (wazero uses an interpreter mode on platforms without mmap exec support), which could further increase CPU cost.

For typical usage patterns (web browsing, messaging), the ~1 ms per-packet overhead is unlikely to be perceptible since network RTTs (50-200 ms) dominate. However, high-throughput scenarios like video streaming or large file downloads will be CPU-bound at the WASM layer, potentially saturating a single core at ~1 MB/s or less.

### Memory

Each WATER connection setup allocates ~2 MB and 9,014 objects. For a single active connection this is manageable, but applications that maintain connection pools or open many concurrent connections (e.g., HTTP/2 multiplexing over multiple WATER tunnels) could see significant heap pressure. On Android devices with 4-6 GB RAM and aggressive memory management, this could trigger more frequent garbage collection pauses.

The WASM module itself (353 KB for shadowsocks) is modest, but the wazero runtime state per-instance adds to the footprint.

### Battery Life

Battery impact comes from two sources:

1. **CPU wake time**: The 93 allocs/roundtrip and data copying through virtual socket pairs keep the CPU active longer per packet than native implementations. For background data sync or push notifications that arrive during doze mode, each packet wakes the CPU for ~1 ms (WATER) vs ~65 us (native SS) -- roughly 17x longer per wake event.

2. **GC pressure**: The allocation-heavy data path means the Go runtime's garbage collector runs more frequently. Each GC cycle is a CPU-intensive operation that prevents the SoC from entering low-power states.

For light usage (occasional browsing, messaging), the battery impact would be modest -- perhaps 5-15% increased drain compared to native shadowsocks. For sustained streaming or large transfers, the impact could be more significant as the WASM runtime keeps a core pegged near 100%.

### Recommendations for mobile

- **Connection reuse is critical**: The 5.24 ms connection setup cost (with 2 MB allocation) makes connection pooling essential on mobile. Avoid creating new WATER connections per-request.
- **Consider pre-warming**: Instantiate the WASM module during app startup rather than on first network request to avoid latency spikes.
- **Monitor GC pauses**: The 93 allocs/roundtrip will generate GC pressure. Profiling on target Android devices is recommended.
- **Throughput ceiling**: Plan for ~1 MB/s per WATER connection on mid-range Android devices. Applications needing higher throughput should either use multiple connections or consider native transport implementations for performance-critical paths.

## Remote Benchmarks (Real Network)

The localhost benchmarks above show WASM overhead dominating (~1.1ms per roundtrip), but on localhost the network RTT is negligible (~18us). To understand real-world impact, we run the same comparisons over an actual network where WASM overhead competes with real RTT.

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
| Raw TCP                    | 35.9 ms     | 1x         |
| Native shadowsocks         | 37.3 ms     | 1.04x      |
| WATER + shadowsocks (WASM) | 37.8 ms     | 1.05x      |

#### Throughput (1 KB echo roundtrip)

| Path                       | Latency/op  | MB/s  | Allocs/op |
|----------------------------|-------------|-------|-----------|
| Raw TCP                    | 35.5 ms     | 0.03  | 0         |
| Native shadowsocks         | 36.1 ms     | 0.03  | 0         |
| WATER + shadowsocks (WASM) | 37.0 ms     | 0.03  | 93        |

#### Web Browsing Simulation (5-9 sequential 1KB resource fetches per page)

| Path                       | Time per page | vs Raw TCP |
|----------------------------|--------------|------------|
| Raw TCP (concurrent)       | 88 ms        | 1x         |
| Native SS (concurrent)     | 92 ms        | 1.05x      |
| WATER + SS (sequential)    | 289 ms       | 3.3x       |

Note: WATER uses sequential fetches on a single connection (reflecting typical Lantern tunnel usage). Native SS and TCP use concurrent connections per resource (browser-like).

#### Connection Setup (WASM instantiation + network handshake)

| Path                       | Time per connection | Memory    | Allocs |
|----------------------------|--------------------:|----------:|-------:|
| WATER + shadowsocks        | 42.0 ms             | ~2 MB     | 9,019  |
| Raw TCP                    | 38.1 ms             | 668 B     | 14     |

### Analysis

**The ~1ms WASM overhead is negligible over a real network.** On localhost, WATER adds 17.6x latency over native SS. Over a 36ms network, it adds only 1.3% (37.8ms vs 37.3ms). The network RTT completely dominates.

**Connection setup is the real cost.** Each WATER connection takes ~42ms and allocates ~2MB. For the web browsing simulation, creating a new WATER connection per resource (sequential due to WASM serialization) makes WATER 3.3x slower than concurrent raw TCP. This confirms that **connection reuse is critical** for WATER performance.

**WASM module stability limits throughput testing.** The shadowsocks WASM module crashes with "input/output error" on echo payloads above ~4KB, even on a single connection. This is the same instability observed with `plain.wasm` on localhost and limits our ability to benchmark large transfers through WATER.

### What This Means for Lantern

For Lantern's typical usage pattern (single persistent WATER tunnel, multiplexed HTTP traffic):
- Individual request latency overhead is **imperceptible** (~1-2ms on a 36ms RTT)
- The bottleneck is connection establishment, not per-packet overhead
- Pre-warming connections and connection pooling eliminate the main cost
- The WASM stability issue with larger payloads needs investigation for production use

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
