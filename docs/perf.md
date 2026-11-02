**Hardware:** AMD Ryzen 5 PRO 5650U with Radeon Graphics, 12 threads, 14.9 GiB RAM, Zorin OS 18.1  
**Run:** 2026-10-30, go1.26.5, median of 3 runs (min to max in brackets), seed 1, files in `/tmp/offgrid-perf-1295714907` (ext4), CPU governor `performance`. No model is involved in these numbers.

**Store** (one SQLite file, `synchronous=FULL`: every write is on disk when it returns):

| What | Ops | Time (s) | Ops/s | File (MiB) | Peak Go heap (MiB) | Peak RSS (MiB) |
|---|---|---|---|---|---|---|
| append (one durable write per op) | 1,000 | 0.999 [0.996 to 1.00] | 1,001 | 0.3 | 3.6 | 21 |
| open an existing log | 100 | 0.0009 [0.0007 to 0.0011] | - | 0.1 | 0.9 | 21 |
| rebuild the view from the log | 100 | 0.0094 [0.0087 to 0.010] | 10,653 | 0.1 | 1.4 | 20 |
| open an existing log | 1,000 | 0.0007 [0.0007 to 0.0010] | - | 0.3 | 0.9 | 23 |
| rebuild the view from the log | 1,000 | 0.074 [0.071 to 0.076] | 13,480 | 0.3 | 3.2 | 24 |
| open an existing log | 10,000 | 0.0007 [0.0007 to 0.0009] | - | 2.3 | 1.0 | 24 |
| rebuild the view from the log | 10,000 | 0.663 [0.658 to 0.691] | 15,080 | 2.3 | 5.2 | 27 |

**Sync** (the replica that catches up must end with the same state hash as its source, or the run fails):

| What | Ops | Time (s) | Ops/s | HTTP requests | Peak Go heap (MiB) | Peak RSS (MiB) |
|---|---|---|---|---|---|---|
| new replica, in process | 100 | 0.0084 [0.0080 to 0.0085] | 11,850 | - | 1.6 | 23 |
| new replica, HTTP on localhost | 100 | 0.011 [0.0096 to 0.011] | 9,467 | 2 | 1.9 | 22 |
| 10 new ops on a 100-op log, HTTP | 10 | 0.0027 [0.0027 to 0.0034] | 3,664 | 2 | 1.3 | 23 |
| new replica, in process | 1,000 | 0.078 [0.074 to 0.083] | 12,834 | - | 3.6 | 25 |
| new replica, HTTP on localhost | 1,000 | 0.079 [0.077 to 0.080] | 12,735 | 3 | 3.5 | 25 |
| 10 new ops on a 1,000-op log, HTTP | 10 | 0.0032 [0.0030 to 0.0033] | 3,170 | 2 | 1.2 | 24 |
| new replica, in process | 10,000 | 0.838 [0.832 to 0.848] | 11,937 | - | 3.6 | 28 |
| new replica, HTTP on localhost | 10,000 | 0.888 [0.887 to 0.893] | 11,258 | 21 | 3.7 | 28 |
| 10 new ops on a 10,000-op log, HTTP | 10 | 0.0089 [0.0082 to 0.012] | 1,126 | 2 | 1.2 | 27 |

**Queue** (jobs that do nothing, so this is the queue's own cost: claim, run, complete, each a durable write):

| Jobs | At a time | Enqueue (s) | Drain (s) | Jobs/s drained | Peak Go heap (MiB) | Peak RSS (MiB) |
|---|---|---|---|---|---|---|
| 100 | 1 | 0.087 [0.081 to 0.087] | 0.186 [0.181 to 0.197] | 539 | 2.7 | 25 |
| 100 | 4 | 0.089 [0.084 to 0.091] | 0.182 [0.181 to 0.193] | 550 | 2.7 | 25 |
| 1,000 | 1 | 0.763 [0.759 to 0.766] | 1.70 [1.70 to 5.21] | 588 | 3.5 | 27 |
| 1,000 | 4 | 4.00 [4.00 to 4.03] | 8.19 [8.18 to 8.23] | 122 | 3.2 | 26 |

Peak RSS is the largest resident memory of the whole process during that row (counter reset before each row). Peak Go heap is sampled every 10 ms, so a very short spike can be missed. Sync over HTTP runs on one machine: it measures protocol, signing and storage cost, not a WiFi link.
