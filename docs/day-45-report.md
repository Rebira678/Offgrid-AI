# Day 45: gate output, before against after

`perfgate -old before.txt -new after.txt`, run on the Day 45 baseline (count=10, 2-vCPU container). (The "missing" benchmark below is `CatchUp10/log-100000`: the before run still had it; the suite pattern was tightened afterwards to leave it out, see the limits in day-45.md.) The files themselves: `perf/baseline-day45-before.txt` and `perf/baseline-day45-after.txt`.

### Improvements

| benchmark | metric | old | new | change | p |
|---|---|---|---|---|---|
| store/sqlstore.ApplyRemote1000/signed=false | ns/op | 62.05 ms | 28.97 ms | -53.3% | 0.000 |
| store/sqlstore.Rebuild/1000-ops | ns/op | 34.16 ms | 17.28 ms | -49.4% | 0.000 |
| store/sqlstore.Rebuild/10000-ops | ns/op | 347.69 ms | 179.62 ms | -48.3% | 0.000 |
| store/sqlstore.ApplyRemoteBatch500 | ns/op | 36.26 ms | 23.53 ms | -35.1% | 0.000 |
| store/sqlstore.ApplyRemote1000/signed=true | ns/op | 128.84 ms | 87.21 ms | -32.3% | 0.000 |
| store/sqlstore.VerifyChain10k | B/op | 8.62 MiB | 6.15 MiB | -28.7% | 0.000 |
| store/sqlstore.VerifyChain10k | allocs/op | 289053 | 219257 | -24.1% | 0.000 |
| store/sqlstore.CatchUp10/log-10000 | ns/op | 1.44 ms | 1.16 ms | -19.5% | 0.000 |
| syncer.SyncRoundTrip | ns/op | 708.27 µs | 582.03 µs | -17.8% | 0.009 |
| store/sqlstore.ApplyRemote1000/signed=false | allocs/op | 158978 | 134248 | -15.6% | 0.000 |
| store/sqlstore.ApplyRemote1000/signed=false | B/op | 7.53 MiB | 6.37 MiB | -15.4% | 0.000 |
| store/sqlstore.ApplyRemoteBatch500 | allocs/op | 79468 | 67222 | -15.4% | 0.000 |
| store/sqlstore.ApplyRemoteBatch500 | B/op | 3.81 MiB | 3.24 MiB | -15.1% | 0.000 |
| store/sqlstore.ApplyRemote1000/signed=true | allocs/op | 258800 | 227115 | -12.2% | 0.000 |
| store/sqlstore.ApplyRemote1000/signed=true | B/op | 12.08 MiB | 10.73 MiB | -11.2% | 0.000 |
| store/sqlstore.CatchUp10/log-10000 | allocs/op | 2177 | 1954 | -10.2% | 0.000 |
| store/sqlstore.Rebuild/1000-ops | allocs/op | 117691 | 108334 | -8.0% | 0.000 |
| store/sqlstore.Rebuild/10000-ops | allocs/op | 1184008 | 1090089 | -7.9% | 0.000 |

102 metrics unchanged or within noise. **Missing from this run:** store/sqlstore.CatchUp10/log-100000.

**PASSED.**
