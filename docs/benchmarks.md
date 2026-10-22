# Benchmarks

Reference results for the read path (NFR-3) and the write and rebuild paths, with the
targets from the implementation plan (milestone 07) and how each one stands.

## Reference machine

- CPU: AMD Ryzen 9 5900HX (8 cores, 16 threads), linux/amd64
- Go: go1.27.1
- Date: 2026-10-22
- Command: `go test -run '^$' -bench . -benchmem -count 6 .`, and
  `-bench BenchmarkGetParallel -cpu 1,4,16` for the scaling curve; medians by `benchstat`.

The value entity (`point`) has only value fields, so `Clone` is a plain copy. The slice
entity (`tagged`) holds a four-element slice, so its clone allocates.

## Results

| Benchmark | Time/op | Bytes/op | Allocs/op |
| --- | --- | --- | --- |
| `Get/size=1000` (value entity) | 37.9 ns | 0 | 0 |
| `Get/size=100000` (value entity) | 42.6 ns | 0 | 0 |
| `GetTagged` (slice entity) | 37.9 ns | 32 | 1 |
| `Lookup/hit` | 14.6 ns | 0 | 0 |
| `Lookup/miss` (authoritative `Absent`) | 13.9 ns | 0 | 0 |
| `List/size=1000` | 1.37 µs | 8192 | 1 |
| `List/size=10000` | 8.43 µs | 81920 | 1 |
| `GetDuringMutations` (parallel reads, one mutator) | 135 ns | 2 | 0 |
| `Mutate` (mutation and refresh against the in-memory store) | 7.86 µs | 1288 | 23 |
| `Rebuild/size=1000` | 223 µs | 359 KiB | 1037 |
| `Rebuild/size=100000` | 30.7 ms | 39 MiB | 100811 |
| `BaselineRWMutexMap` (plain `RWMutex` map read) | 13.5 ns | 0 | 0 |

`GetParallel/size=100000` scaling (time per read across all goroutines):

| `-cpu` | Time/op | Throughput vs `-cpu 1` |
| --- | --- | --- |
| 1 | 38.1 ns | 1.0× |
| 4 | 66.6 ns | 0.6× |
| 16 | 30.0 ns | 1.3× |

## Targets

| Target | Status |
| --- | --- |
| `Get` and `Lookup` on the value entity: 0 allocs/op | Met. Asserted by `TestGet_AllocsOnlyClone` and `TestLookup_ZeroAllocs`. |
| `List`: 1 alloc/op | Met. Asserted by `TestList_OneAlloc`. |
| `Get` within 1.5× of `BaselineRWMutexMap` single-threaded | **Not met**: about 2.8× (37.9 ns against 13.5 ns). `Lookup` is within 1.1×. Without the clone `Get` measures 29 ns; the clone is an indirect call through `Spec.Clone`, and the rest is the safety checks and the generic bounded-map wrapper. |
| `GetParallel` throughput at `-cpu 16` at least 4× of `-cpu 1` | **Not met**: 1.3×. Every read takes the one `sync.RWMutex` read lock, whose reader count is a single contended cache line. |

Per decision D6 the read path stays on one `RWMutex` in v1. The missed targets are
tracked in a GitHub issue that evaluates a copy-on-write read path (design section 12);
it is not implemented in v1.

## Reproducing

```sh
go test -run '^$' -bench . -benchmem -count 6 . | tee new.txt
go test -run '^$' -bench BenchmarkGetParallel -benchmem -count 6 -cpu 1,4,16 . | tee -a new.txt
benchstat new.txt
```

CI runs the benchmarks on every pull request against the base branch and posts the
`benchstat` comparison to the job summary. The comparison is informational; the
allocation tests (`make alloc`) are the hard gate.
