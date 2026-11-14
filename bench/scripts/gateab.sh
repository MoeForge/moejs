#!/bin/sh
# gateab.sh <bindir> <base tree> <head tree> <outdir> [rounds]: the
# interleaved A/B of the plugin-workload gate (docs/DESIGN.md §14.2) with
# the gatebuild.sh binaries <bindir>/<kind>-{base,head}.test, each run in
# the tree it was built from (the tests read its testdata; both trees need
# the plugins, bench/testdata/plugins/fetch.sh). Default 10 rounds; odd
# rounds run base first, even rounds head first, so a drift in clock speed
# cancels out. Then
#   benchstat <outdir>/base.txt <outdir>/head.txt
#
# ROWS=compile runs only the plugins' compile (BenchmarkCompile/moejs), the
# rows of a syntax or compiler change that leaves the plugins' bytecode as
# it was. BENCHTIME (default 0.5s) is -test.benchtime.
#
# Run it under the heavy lock and pinned to the P cores (taskset -c 0-7 on
# this machine), after one discarded round: the first rounds after a build
# run at a higher clock than the rest.
#
# PADS="<bytes>..." averages over where the engine is linked. Build each
# tree once per pad, named <base|head>-p<bytes>:
#   PAD=24512 gatebuild.sh <tree> <bindir> base-p24512
# Round i then runs the binaries of the ((i - 1) mod n)-th of the n pads,
# the same pad for base and head; give a multiple of n rounds, and
# benchstat compares the rows over all placements. Rows bound by the CPU's
# front end (TryCatchThrow, PluginHook, ModuleInstantiate) move by 5-20 %
# with the engine's placement alone (gatebuild.sh), so a one-placement A/B
# of a change that leaves their instructions alone measures where the
# change happened to put the engine, not the change.
B=$1 BT=$2 HT=$3 O=$4 R=${5:-10}
BTIME=${BENCHTIME:-0.5s}
mkdir -p "$O"
: >"$O/base.txt"
: >"$O/head.txt"
suffix() { # suffix <round>: S, the binary name suffix of the round's pad
	S=
	[ -n "${PADS:-}" ] || return 0
	k=$1
	set -- $PADS
	k=$(((k - 1) % $# + 1))
	eval "S=-p\${$k}"
}
one() { # one <base|head> <tree>
	f="$O/$1.txt"
	if [ "${ROWS:-full}" = compile ]; then
		(cd "$2/bench" && BENCH_ENGINES=moejs "$B/bench-$1$S.test" -test.run xxx -test.benchmem -test.benchtime "$BTIME" -test.bench '^BenchmarkCompile$/^moejs$/') >>"$f" 2>&1
		want=BenchmarkCompile/
	else
		(cd "$2/engine" && "$B/engine-$1$S.test" -test.run xxx -test.benchmem -test.benchtime "$BTIME" -test.bench '^Benchmark(Call|CallJSToJS|LoopSum|ForOfArray|TryCatchThrow|ObjectAssign|PropertyGetMonomorphic|PropertyGetPrototype|PluginHook|ModuleInstantiate|ArrayMap)$') >>"$f" 2>&1
		(cd "$2/tests" && "$B/root-$1$S.test" -test.run xxx -test.benchmem -test.benchtime "$BTIME" -test.bench '^BenchmarkCall$') >>"$f" 2>&1
		(cd "$2/bench" && BENCH_ENGINES=moejs "$B/bench-$1$S.test" -test.run xxx -test.benchmem -test.benchtime "$BTIME" -test.bench '^Benchmark(HookSuite|NewRuntime)$|^BenchmarkHostFlow$/^moejs$/./^all$|^BenchmarkInstantiate$/^moejs$/^(alibaba|sora)$|^BenchmarkMicro$/./^moejs$') >>"$f" 2>&1
		want=BenchmarkHookSuite/
	fi
	# The plugin rows skip without the plugins; an A/B without them is no gate.
	if ! grep -q "^$want" "$f"; then
		echo "$1: no $want rows in $f (are the plugins fetched in $2?)" >&2
		exit 1
	fi
}
for i in $(seq "$R"); do
	suffix "$i"
	if [ $((i % 2)) = 1 ]; then
		one base "$BT"
		one head "$HT"
	else
		one head "$HT"
		one base "$BT"
	fi
	echo "round $i done${S:+ ($S)}"
done
