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
```

The shadowsocks WASM module is loaded from `../wateringhole/protocols/shadowsocks/v1.0.0/shadowsocks_client.wasm`. Clone [getlantern/wateringhole](https://github.com/getlantern/wateringhole) as a sibling directory and run `git lfs pull`.
