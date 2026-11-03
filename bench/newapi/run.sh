#!/bin/sh
# Runs new-api's JavaScript plugin tests and benchmarks against a moejs
# commit: the check that a change keeps new-api's plugin host working and no
# slower. new-api is fetched read-only at the commit pinned below into a
# cache, copied, and pointed at a `git archive` export of the moejs commit
# with a go.mod replace; the working copy is never built.
#
# Usage, from anywhere in a moejs checkout (a rev is any commit name git
# accepts):
#
#   bench/newapi/run.sh test [rev]
#       go test -count=1 over new-api's pkg/jsplugin, relay/channel/task/
#       jsplugin and plugins against moejs at rev (default HEAD). RACE=1 adds
#       a -race run of the same packages; HTTP=1 adds the HTTP-level plugin
#       tests of middleware, router and controller.
#   bench/newapi/run.sh ab <base> <head> <outdir>
#       new-api's BenchmarkTaskPlugin* with moejs at base and at head, from
#       test binaries linked with -ldflags=-funcalign=64, as the engine's
#       gate binaries are (bench/scripts/gatebuild.sh, docs/DESIGN.md
#       §14.2: every function on a 64-byte boundary, so a change moves
#       only its own code), interleaved: one discarded warm-up round, then
#       ROUNDS rounds, base first in odd rounds and head first in even
#       ones, -benchmem; then the allocation pass, base, head, base,
#       head, with the collector off, one P and ALLOCS_N iterations, where
#       B/op and allocs/op do not depend on when the collector runs; then
#       the report.
#   bench/newapi/run.sh report <outdir>
#       benchstat of the rounds in outdir, base against head, a table of
#       B/op and allocs/op from the rounds and from the allocation pass side
#       by side, and the gate. It fails on:
#       - an ns/op increase of more than 3 % with p < 0.05;
#       - an allocs/op of the allocation pass above every base run;
#       - a B/op of the allocation pass more than 64 bytes above every base
#         run (its runs differ by up to 32 bytes: map layouts);
#       - a B/op or allocs/op of the rounds more than 3 % up with p < 0.05
#         and above every base round. The pass cannot see what the code
#         allocates again after a collection, such as the refill of a
#         sync.Pool (a collection drains the pools, and the pass runs none):
#         a third of Submit/8192KiB's B/op in the rounds. The rounds see it,
#         through the collector's noise (that row's B/op spreads 37 % over
#         20 rounds): the rule fails about one A/B of equal code in 200,
#         and fails a +30 % row (a pooled 8 MiB buffer doubled) every time.
#       Any other rise of a median is listed as a warning.
#
# Env knobs:
#   GO            go command (default go). new-api's go.mod says go 1.25.1;
#                 a newer toolchain builds it with GOTOOLCHAIN=local.
#   NEWAPI_CACHE  git directory holding the pinned new-api commit
#                 (default $HOME/.cache/moejs/new-api)
#   WORK          exports, test binaries and test logs
#                 (default ${TMPDIR:-/tmp}/moejs-newapi)
#   ROUNDS        A/B rounds (default 10; benchstat needs 6 or more to call
#                 anything significant). ROUND=k runs round k alone (0 is
#                 the warm-up, a the allocation pass) and skips the report,
#                 for a caller that cuts the A/B into short jobs; run report
#                 afterwards.
#   ALLOCS_N      iterations of each benchmark in the allocation pass
#                 (default 20). With no collector, one process keeps all it
#                 allocates: about ALLOCS_N times the rows' B/op added up,
#                 0.5 GiB at 20 with the default BENCH.
#   BENCH         benchmark regexp (default BenchmarkTaskPlugin)
#   BENCHTIME     -benchtime of each run (default 1s)
#   CPUS, PROCS   run each benchmark under taskset -c $CPUS and with
#                 GOMAXPROCS=$PROCS (default: unpinned)
#   LOCKS         lock files, separated by spaces, taken with flock in that
#                 order around each test run, each build of a test binary
#                 and each A/B round (default none; on a machine shared with
#                 other benchmarks, the lock files they take, such as
#                 "/tmp/moejs-heavy.lock $HOME/moejs-bench/lock")
#   BENCHSTAT     benchstat command (default benchstat;
#                 go install golang.org/x/perf/cmd/benchstat@latest)
set -eu

# The new-api commit the gate runs; bump it when new-api's suite changes.
NEWAPI_REPO=https://github.com/QuantumNous/new-api
NEWAPI_REV=b48b74ab77cf289ccaaa8d99c067ce50cced4a82
# The moejs module path new-api imports at NEWAPI_REV. moejs's own path is
# read from the go.mod of the commit tested: where the two differ (moejs
# moved from github.com/Calcium-Ion/moejs to github.com/MoeForge/moejs),
# prepare rewrites the imports of its copy of new-api to moejs's path.
NEWAPI_MOEJS=github.com/Calcium-Ion/moejs
PACKAGES="./pkg/jsplugin/... ./relay/channel/task/jsplugin/... ./plugins/..."
HTTP_PACKAGES="./middleware/ ./router/ ./controller/"
HTTP_RUN='TaskPlugin|PluginRouter|Distributor|BuiltIn'

GO=${GO:-go}
NEWAPI_CACHE=${NEWAPI_CACHE:-$HOME/.cache/moejs/new-api}
WORK=${WORK:-${TMPDIR:-/tmp}/moejs-newapi}
ROUNDS=${ROUNDS:-10}
ALLOCS_N=${ALLOCS_N:-20}
BENCH=${BENCH:-BenchmarkTaskPlugin}
BENCHTIME=${BENCHTIME:-1s}
BENCHSTAT=${BENCHSTAT:-benchstat}
LOCKS=${LOCKS:-}
REPO=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
# The replace is not in new-api's go.sum.
GOFLAGS="${GOFLAGS:+$GOFLAGS }-mod=mod"
GIN_MODE=release
export GOFLAGS GIN_MODE

die() {
	echo "run.sh: $*" >&2
	exit 2
}

# locked <lock files> <command...> runs the command holding every lock.
locked() {
	ls=$1
	shift
	if [ -z "$ls" ]; then
		"$@"
		return
	fi
	command -v flock >/dev/null 2>&1 || die "LOCKS needs flock"
	first=${ls%% *}
	rest=${ls#"$first"}
	mkdir -p "$(dirname "$first")" || die "$first: cannot create its directory"
	(flock 9 && locked "${rest# }" "$@") 9>"$first"
}

# pinned <procs> <command...> runs the command under CPUS and with
# GOMAXPROCS=procs if procs is not empty.
pinned() {
	procs=$1
	shift
	if [ -n "${CPUS:-}" ]; then
		set -- taskset -c "$CPUS" "$@"
	fi
	if [ -n "$procs" ]; then
		set -- env GOMAXPROCS="$procs" "$@"
	fi
	"$@"
}

fetch_newapi() {
	if git --git-dir="$NEWAPI_CACHE" cat-file -e "$NEWAPI_REV^{commit}" 2>/dev/null; then
		return
	fi
	mkdir -p "$NEWAPI_CACHE"
	git init -q --bare "$NEWAPI_CACHE"
	git --git-dir="$NEWAPI_CACHE" fetch -q --depth 1 "$NEWAPI_REPO" "$NEWAPI_REV"
	git --git-dir="$NEWAPI_CACHE" cat-file -e "$NEWAPI_REV^{commit}"
}

# prepare <rev> sets tree to a directory holding moejs/, the export of rev,
# and newapi/, the pinned new-api using it. The export is made once per
# commit.
prepare() {
	sha=$(git -C "$REPO" rev-parse --verify --quiet "$1^{commit}") || die "$1: not a commit"
	tree=$WORK/newapi-$(printf %.12s "$NEWAPI_REV")-moejs-$sha
	if [ -f "$tree/ready" ]; then
		return
	fi
	fetch_newapi
	rm -rf "$tree"
	mkdir -p "$tree/moejs" "$tree/newapi"
	git -C "$REPO" archive "$sha" | tar -x -C "$tree/moejs"
	git --git-dir="$NEWAPI_CACHE" archive "$NEWAPI_REV" | tar -x -C "$tree/newapi"
	mod=$(sed -n 's/^module[[:space:]]*//p' "$tree/moejs/go.mod")
	[ -n "$mod" ] || die "$sha: go.mod has no module line"
	if [ "$mod" != "$NEWAPI_MOEJS" ]; then
		find "$tree/newapi" -name '*.go' -exec sed -i.orig -e "s#\"$NEWAPI_MOEJS\"#\"$mod\"#g" -e "s#\"$NEWAPI_MOEJS/#\"$mod/#g" {} +
		find "$tree/newapi" -name '*.go.orig' -exec rm -f {} +
		(cd "$tree/newapi" && "$GO" mod edit -droprequire "$NEWAPI_MOEJS" -require "$mod@v0.0.0-00010101000000-000000000000")
	fi
	(cd "$tree/newapi" && "$GO" mod edit -replace "$mod=$tree/moejs")
	: >"$tree/ready"
}

# gotest <log> <go test arguments...> runs go test -v in tree/newapi and
# prints the package results and the count of top-level tests.
gotest() {
	log=$1
	shift
	echo "== go test $*"
	st=0
	(cd "$tree/newapi" && locked "$LOCKS" "$GO" test -count=1 -v "$@") >"$log" 2>&1 || st=$?
	grep -E '^(ok|FAIL|---)[[:space:]]' "$log" | grep -v '^--- PASS' || true
	echo "top-level tests: $(grep -c '^--- PASS' "$log") passed, $(grep -c '^--- FAIL' "$log") failed, $(grep -c '^--- SKIP' "$log") skipped; log $log"
	if [ "$st" != 0 ]; then
		echo "FAIL (exit $st)"
		failed=1
	fi
}

cmd_test() {
	prepare "${1:-HEAD}"
	echo "new-api $NEWAPI_REV, moejs $sha"
	failed=0
	# The package lists split on spaces.
	gotest "$tree/test.log" $PACKAGES
	if [ "${RACE:-}" = 1 ]; then
		gotest "$tree/race.log" -race $PACKAGES
	fi
	if [ "${HTTP:-}" = 1 ]; then
		gotest "$tree/http.log" -run "$HTTP_RUN" $HTTP_PACKAGES
	fi
	[ "$failed" = 0 ] || exit 1
	echo PASS
}

# build <rev> prepares rev and builds its relay test binary, which holds the
# benchmarks, with -funcalign=64; it sets tree. A binary built otherwise (by
# an earlier version of this script) is built again.
build() {
	prepare "$1"
	if [ ! -x "$tree/relay.test" ] || [ ! -f "$tree/funcalign64" ]; then
		(cd "$tree/newapi" && locked "$LOCKS" "$GO" test -c -ldflags=-funcalign=64 -o "$tree/relay.test" ./relay/channel/task/jsplugin)
		: >"$tree/funcalign64"
	fi
}

# bench <base|head> <file> [procs benchtime] appends one run of the side's
# benchmarks.
bench() {
	eval "dir=\$${1}_tree"
	(cd "$dir/newapi/relay/channel/task/jsplugin" &&
		pinned "${3-${PROCS:-}}" "$dir/relay.test" -test.run '^$' -test.bench "$BENCH" -test.benchmem -test.benchtime "${4:-$BENCHTIME}") >"$2.tmp" 2>&1 ||
		{
			cat "$2.tmp" >&2
			die "$1 benchmarks failed"
		}
	grep '^Benchmark' "$2.tmp" >>"$2"
	rm -f "$2.tmp"
}

# round <k> runs round k into out/r<k>-{base,head}.txt; round a is the
# allocation pass, into out/allocs-{base,head}.txt.
round() {
	if [ "$1" = a ]; then
		rm -f "$out/allocs-base.txt" "$out/allocs-head.txt"
		for side in base head base head; do
			(GOGC=off && export GOGC && bench $side "$out/allocs-$side.txt" 1 "${ALLOCS_N}x") || exit 1
		done
		echo "allocation pass done $(date +%H:%M:%S)"
		return
	fi
	rm -f "$out/r$1-base.txt" "$out/r$1-head.txt"
	if [ $(($1 % 2)) = 0 ] && [ "$1" != 0 ]; then
		bench head "$out/r$1-head.txt"
		bench base "$out/r$1-base.txt"
	else
		bench base "$out/r$1-base.txt"
		bench head "$out/r$1-head.txt"
	fi
	echo "round $1 done $(date +%H:%M:%S)"
}

cmd_ab() {
	[ $# = 3 ] || die "usage: run.sh ab <base> <head> <outdir>"
	build "$1"
	base_tree=$tree base_sha=$sha
	build "$2"
	head_tree=$tree head_sha=$sha
	mkdir -p "$3"
	out=$(cd "$3" && pwd)
	# Once per A/B: a run of all rounds, or its warm-up when cut into
	# rounds.
	if [ -z "${ROUND:-}" ] || [ "$ROUND" = 0 ] || [ ! -f "$out/env.txt" ]; then
		{
			echo "new-api $NEWAPI_REV"
			echo "base $1 = moejs $base_sha"
			echo "head $2 = moejs $head_sha"
			echo "$("$GO" version), bench $BENCH, benchtime $BENCHTIME, CPUS=${CPUS:-} PROCS=${PROCS:-}, $(date)"
		} >"$out/env.txt"
	fi
	if [ -n "${ROUND:-}" ]; then
		locked "$LOCKS" round "$ROUND"
		return
	fi
	i=0
	while [ "$i" -le "$ROUNDS" ]; do
		locked "$LOCKS" round "$i"
		i=$((i + 1))
	done
	locked "$LOCKS" round a
	cmd_report "$out"
}

cmd_report() {
	[ $# = 1 ] || die "usage: run.sh report <outdir>"
	cd "$1"
	[ -f allocs-base.txt ] && [ -f allocs-head.txt ] || die "$1: no allocation pass (ROUND=a)"
	: >base.txt
	: >head.txt
	for f in r*-base.txt r*-head.txt; do
		[ -f "$f" ] || die "$1: no rounds"
		k=${f%-*}
		[ -f "$k-base.txt" ] || die "$1: round ${k#r} has no base run"
		[ -f "$k-head.txt" ] || die "$1: round ${k#r} has no head run"
		[ "$k" = r0 ] || cat "$f" >>"${f#"$k"-}"
	done
	"$BENCHSTAT" base.txt head.txt >benchstat.txt
	"$BENCHSTAT" -format csv base.txt head.txt >benchstat.csv 2>/dev/null
	cat benchstat.txt
	# The input is sections: benchstat's CSV, whose tables each name their
	# unit in their second header line and whose rows are name, base
	# median, CI, head median, CI, delta, "p=P n=N"; then the benchmark
	# lines of the rounds and of the allocation pass, side by side.
	{
		echo "#csv"
		cat benchstat.csv
		for f in base.txt head.txt allocs-base.txt allocs-head.txt; do
			echo "#$f"
			cat "$f"
		done
	} | awk '
		function key(name) {
			sub(/^Benchmark/, "", name)
			sub(/-[0-9]+$/, "", name)
			return name
		}
		function median(list,    v, n, i, j, t) {
			n = split(list, v, " ")
			for (i = 2; i <= n; i++)
				for (j = i; j > 1 && v[j - 1] + 0 > v[j] + 0; j--) { t = v[j]; v[j] = v[j - 1]; v[j - 1] = t }
			return n % 2 ? v[(n + 1) / 2] + 0 : (v[n / 2] + v[n / 2 + 1]) / 2
		}
		function extreme(list, top,    v, n, i, m) {
			n = split(list, v, " ")
			m = v[1] + 0
			for (i = 2; i <= n; i++) if (top ? v[i] + 0 > m : v[i] + 0 < m) m = v[i] + 0
			return m
		}
		function pct(b, h) { return b == 0 ? (h == 0 ? 0 : 100) : (h - b) / b * 100 }
		function note(what, name, unit, d, how) {
			lines = lines sprintf("%s %-40s %-9s %+7.2f%% %s\n", what, name, unit, d, how)
			if (what == "FAIL") bad = 1
		}
		/^#/ { sect = substr($0, 2); next }
		sect == "csv" {
			n = split($0, f, ",")
			if (f[1] == "") { if (f[3] == "CI") unit = f[2]; next }
			if (f[1] == "geomean" || n < 7) next
			p = f[7]; sub(/^p=/, "", p); sub(/ .*/, "", p)
			if (unit == "sec/op") {
				d = pct(f[2] + 0, f[4] + 0)
				if (p + 0 < 0.05 && d > 3) note("FAIL", key(f[1]), "ns/op", d, sprintf("(p=%.3f)", p))
			} else if (unit == "B/op" || unit == "allocs/op") P[key(f[1]), unit] = p + 0
			next
		}
		{
			k = key($1)
			for (i = 2; i < NF; i++)
				if ($(i + 1) == "B/op" || $(i + 1) == "allocs/op") V[sect, k, $(i + 1)] = V[sect, k, $(i + 1)] " " $i
			if (!(k in seen)) { seen[k] = 1; order[++rows] = k }
		}
		END {
			printf "\n%-40s %-9s %25s %20s\n", "", "", "rounds (collector on)", "pass (collector off)"
			printf "%-40s %-9s %10s %8s %5s %10s %8s\n", "B/op and allocs/op, medians", "", "base", "head", "p", "base", "head"
			for (r = 1; r <= rows; r++) {
				k = order[r]
				for (u = 0; u < 2; u++) {
					unit = u ? "allocs/op" : "B/op"
					rb = V["base.txt", k, unit]; rh = V["head.txt", k, unit]
					ab = V["allocs-base.txt", k, unit]; ah = V["allocs-head.txt", k, unit]
					if (rb == "" || rh == "" || ab == "" || ah == "") {
						note("FAIL", k, unit, 0, "(missing from a side)")
						continue
					}
					p = ((k, unit) in P) ? P[k, unit] : 1
					drnd = pct(median(rb), median(rh)); dpass = pct(median(ab), median(ah))
					printf "%-40s %-9s %10.0f %+7.2f%% %5.3f %10.0f %+7.2f%%\n", k, unit, median(rb), drnd, p, median(ab), dpass
					tol = u ? 0 : 64
					if (extreme(ah, 0) > extreme(ab, 1) + tol) note("FAIL", k, unit, dpass, "(allocation pass)")
					else if (median(ah) > median(ab)) note("warn", k, unit, dpass, "(allocation pass)")
					if (p < 0.05 && drnd > 3 && median(rh) > extreme(rb, 1)) note("FAIL", k, unit, drnd, sprintf("(rounds, p=%.3f)", p))
					else if (p < 0.05 && drnd > 0) note("warn", k, unit, drnd, sprintf("(rounds, p=%.3f)", p))
				}
			}
			printf "\n%s", lines
			print bad ? "gate: FAIL" : "gate: PASS"
			exit bad
		}
	' >verdict.txt || {
		cat verdict.txt
		exit 1
	}
	cat verdict.txt
}

[ $# -ge 1 ] || die "usage: run.sh test [rev] | ab <base> <head> <outdir> | report <outdir>"
cmd=$1
shift
case $cmd in
test) cmd_test "$@" ;;
ab) cmd_ab "$@" ;;
report) cmd_report "$@" ;;
*) die "unknown command $cmd" ;;
esac
