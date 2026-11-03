#!/usr/bin/env bash
# Runs the bench measurements (benchmarks, the footprint table, an allocation
# profile and the PGO comparison) and writes the raw output to bench/results/
# (git-ignored).
#
# Env knobs:
#   BENCH_LOCK=/path/to/lock  take an flock on this path around each
#                             benchmark block (useful if something else on
#                             the machine competes for CPU); unset runs each
#                             block directly, with no locking.
#   SERIAL_CORES, SERIAL_PROCS  pin the serial rows to a core set with
#                             `taskset -c $SERIAL_CORES env
#                             GOMAXPROCS=$SERIAL_PROCS`, when `taskset` is
#                             available. On Linux, SERIAL_CORES/SERIAL_PROCS
#                             default to the lower half of the machine's
#                             cores; elsewhere (no `taskset`, e.g. macOS) the
#                             serial rows run unpinned unless both are set.
#   PARALLEL_CPUS             the `-cpu` list for the parallel sweep; default
#                             "1,4,<nproc>".
set -euo pipefail
cd "$(dirname "$0")/.."
testdata/plugins/fetch.sh
mkdir -p results

ncpu() {
	if command -v nproc >/dev/null 2>&1; then
		nproc
	elif command -v sysctl >/dev/null 2>&1; then
		sysctl -n hw.ncpu
	else
		getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4
	fi
}

PIN=()
if command -v taskset >/dev/null 2>&1; then
	if [ -z "${SERIAL_CORES:-}" ] && [ "$(uname -s)" = "Linux" ]; then
		SERIAL_CORES="0-$(( $(ncpu) / 2 - 1 ))"
		SERIAL_PROCS="${SERIAL_PROCS:-$(( $(ncpu) / 2 ))}"
	fi
	if [ -n "${SERIAL_CORES:-}" ] && [ -n "${SERIAL_PROCS:-}" ]; then
		PIN=(taskset -c "$SERIAL_CORES" env "GOMAXPROCS=$SERIAL_PROCS")
	fi
fi
PARALLEL_CPUS="${PARALLEL_CPUS:-1,4,$(ncpu)}"

maybe_flock() {
	if [ -n "${BENCH_LOCK:-}" ]; then
		flock "$BENCH_LOCK" "$@"
	else
		"$@"
	fi
}

go version | tee results/env.txt
if command -v lscpu >/dev/null 2>&1; then
	lscpu | tee -a results/env.txt
else
	sysctl -n machdep.cpu.brand_string 2>/dev/null | tee -a results/env.txt || true
fi
echo "SERIAL_CORES=${SERIAL_CORES:-} SERIAL_PROCS=${SERIAL_PROCS:-} PARALLEL_CPUS=$PARALLEL_CPUS" | tee -a results/env.txt
date -u +%Y-%m-%dT%H:%M:%SZ | tee -a results/env.txt

run() { local name=$1; shift; echo "== $name"; maybe_flock "$@" 2>&1 | tee "results/$name.txt" | grep -E 'Benchmark|^(ok|FAIL|---)' || true; }

run newruntime  "${PIN[@]}" go test -run xxx -bench 'BenchmarkNewRuntime' -benchmem -count 5 .
run compile     "${PIN[@]}" go test -run xxx -bench 'BenchmarkCompile' -benchmem -count 5 .
run instantiate "${PIN[@]}" go test -run xxx -bench 'BenchmarkInstantiate' -benchmem -count 5 .
run hooksuite   "${PIN[@]}" go test -run xxx -bench 'BenchmarkHookSuite$' -benchmem -count 5 .
run parallel    go test -run xxx -bench 'BenchmarkHookSuiteParallel' -benchmem -count 3 -cpu "$PARALLEL_CPUS" .
# v8go crashes when two of its isolates are created or run concurrently, so it
# cannot be driven from several goroutines; it is gated behind V8_PARALLEL and
# only ever run at -cpu 1 here.
run parallel_v8 env V8_PARALLEL=1 go test -run xxx -bench 'BenchmarkHookSuiteParallel/v8go' -benchmem -count 3 -cpu 1 .
run micro       "${PIN[@]}" go test -run xxx -bench 'BenchmarkMicro' -benchmem -count 5 .
run hostflow    "${PIN[@]}" go test -run xxx -bench 'BenchmarkHostFlow' -benchmem -count 5 .
run microclasses "${PIN[@]}" go test -run xxx -bench 'BenchmarkMicroClasses' -benchmem -count 5 .
run phases      "${PIN[@]}" go test -run xxx -bench 'BenchmarkHookPhases' -benchmem -count 3 -benchtime 300ms .
run hook_cases  "${PIN[@]}" go test -run xxx -bench 'BenchmarkHook$' -benchmem -count 3 -benchtime 100ms .
run footprint   go test -run TestFootprint -v .

echo "== memprofile (allocation attribution, moejs hook suite)"
maybe_flock "${PIN[@]}" go test -run xxx -bench 'BenchmarkHookSuite$/moejs$' -benchmem -memprofile results/moejs.mem.prof -memprofilerate 1 -o results/bench.test . | tee results/memprofile.txt
go tool pprof -sample_index=alloc_objects -top -nodecount=25 results/bench.test results/moejs.mem.prof | tee results/alloc_objects_top.txt
go tool pprof -sample_index=alloc_space -top -nodecount=25 results/bench.test results/moejs.mem.prof | tee results/alloc_space_top.txt

echo "== pgo"
maybe_flock go run -trimpath ./cmd/pgo -duration 30s | tee results/pgo_profile.txt
run hooksuite_pgo_off "${PIN[@]}" go test -run xxx -bench 'BenchmarkHookSuite$/moejs' -benchmem -count 5 -pgo=off .
run hooksuite_pgo_on  "${PIN[@]}" go test -run xxx -bench 'BenchmarkHookSuite$/moejs' -benchmem -count 5 -pgo=../default.pgo .
echo "== done"
