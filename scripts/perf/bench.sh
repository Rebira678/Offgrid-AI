#!/usr/bin/env bash
# Day 45: the performance suite. One command so that the baseline, a contributor's run and CI
# measure the same things the same way.
#
#   scripts/perf/bench.sh OUT.txt            # count=10, the CI setting
#   COUNT=3 scripts/perf/bench.sh OUT.txt    # a quicker, noisier look
#   PROFILE=dir scripts/perf/bench.sh OUT.txt  # also write cpu/mem/mutex profiles per package into dir
#
# The go test default timeout is 10 minutes per package and the store's benchmarks take longer
# than that at count=10, so the script turns the timeout off.
#
# Output is plain `go test -bench` text, which benchstat and cmd/perfgate both read.
# Run it on a quiet machine (nothing else busy, power cable in, governor "performance"), and
# compare only runs made on the same machine: see docs/day-45.md.
set -euo pipefail

out="${1:?usage: scripts/perf/bench.sh OUT.txt}"
count="${COUNT:-10}"
benchtime="${BENCHTIME:-300ms}"
only="${ONLY:-}"    # run only the suite entries whose package contains this text (for a quick look at one package)
append="${APPEND:-}" # add to OUT instead of starting it (used to finish an interrupted run)
cd "${ROOT:-$(dirname "$0")/../..}"   # ROOT lets CI run this one script against a second checkout (the base branch)

# package|regexp of the benchmarks that are part of the suite.
# Left out on purpose: Rebuild/50000 and CatchUp10/log-100000 (minutes to build their logs; docs/day-36.md has them).
suite=(
  "./internal/crdt|."
  "./internal/hlc|Now\$|Update|String|Parse"
  "./internal/jobqueue|."
  "./internal/syncer|."
  "./internal/agent|RulesMatch"
  "./internal/sim|Run"
  "./internal/store/sqlstore|^BenchmarkAppend\$|Rebuild/(1000|10000)|Fields/1000|VerifyChain10k|CatchUp10/log-10000$|Record|ApplyRemote|OpsSince|SignOp|VerifyOp\$|OpHash\$|DeleteTask|RemoveNote|NotesFromOps|AppendConcurrent|ListUnderWrites"
  "./internal/blobstore|."
)

if [ -z "$append" ]; then : >"$out"; fi
{
  echo "# offgrid perf suite: count=$count benchtime=$benchtime $(go version)"
  echo "# host: $(uname -sr) $(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | sed 's/.*: //')"
} >>"$out"

for entry in "${suite[@]}"; do
  pkg="${entry%%|*}"
  re="${entry#*|}"
  if [ -n "$only" ] && [[ "$pkg" != *"$only"* ]]; then continue; fi
  args=(-run '^$' -bench "$re" -benchmem -count "$count" -benchtime "$benchtime" -timeout 0)
  if [ -n "${PROFILE:-}" ]; then
    mkdir -p "$PROFILE"
    name="$(echo "$pkg" | tr '/.' '__')"
    # Profiles are taken in their own pass: the memory profile's sampling slows the run, so the
    # timed numbers above never come from a profiled run.
    go test -run '^$' -bench "$re" -benchtime "$benchtime" -cpuprofile "$PROFILE/$name.cpu" -o "$PROFILE/$name.test" "$pkg" >/dev/null
    go test -run '^$' -bench "$re" -benchtime "$benchtime" -memprofile "$PROFILE/$name.mem" -memprofilerate 4096 -o "$PROFILE/$name.test" "$pkg" >/dev/null
    go test -run '^$' -bench "$re" -benchtime "$benchtime" -mutexprofile "$PROFILE/$name.mutex" -mutexprofilefraction 1 -o "$PROFILE/$name.test" "$pkg" >/dev/null
  fi
  go test "${args[@]}" "$pkg" | grep -E '^(goos|goarch|pkg|cpu|Benchmark)' >>"$out"
done
echo "wrote $out"
