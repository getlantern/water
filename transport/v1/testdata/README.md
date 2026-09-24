# v1 test WATMs

`plain.wasm` and `reverse.wasm` are the `tinygo/v1/examples/plain` and
`tinygo/v1/examples/reverse` modules from
[getlantern/watm](https://github.com/getlantern/watm).

Built 2026-09-23 from `alloc-free-io` @ `4b63fed` (getlantern/watm#2) with
TinyGo 0.40.1 on Go 1.25.9 and binaryen `wasm-opt` 133:

```sh
# from the watm checkout; WATER is the path to this water checkout
for m in plain reverse; do
  (cd tinygo/v1/examples/$m &&
    tinygo build -no-debug -target=wasi -tags=purego \
      -o "$WATER/transport/v1/testdata/$m.wasm" .)
done
```

Rebuild them when the SDK or toolchain changes. Benchmarks and profiles run
against these files, so a stale build measures an old runtime: the previous
files predated TinyGo's current allocator and spent 98% of their guest calls
in GC bookkeeping. Keep `-no-debug`; with DWARF present, wazero parses it on
every `proc_exit(0)` at instantiation.
