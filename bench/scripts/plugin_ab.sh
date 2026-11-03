#!/bin/sh
# plugin_ab.sh <base> <head> [rounds]: the plugin scenarios of
# bench/cmd/pluginbench with moejs at <base> against moejs at <head>. Each
# is a git revision of this repository or the directory of a moejs checkout
# whose module path is github.com/MoeForge/moejs. Both sides run this
# checkout's pluginbench; only the moejs it links differs.
#
# Run bench/testdata/plugins/fetch.sh and bench/scenarios/gen.py first. The
# script then:
#
#   - builds one binary per side, linked with -funcalign=64 (gatebuild.sh
#     says why);
#   - runs every request of every scenario once on both sides and stops if
#     any result differs;
#   - runs the cells in rounds (3 by default), the side that goes first
#     alternating from round to round, and prints the median requests per
#     second of each cell and side and the GC count.
#
# Environment:
#   WORK     directory for the trees, binaries and results (default: a new
#            temporary directory, kept and printed at the end)
#   RUN      command prefix for the timed runs, such as "taskset -c 0-7"
#   ENGINES  default "moejs moejs-nocopy"
#   CELLS    scenario:workers:seconds, default "small:1:6 small:8:6
#            agent100k:1:6 agent100k:8:6 big8m:8:8 multi8x1m:8:8"
#   WARM     warm-up seconds of each run, default 2
#
# The runs are noisy on a shared machine. Read a change below the spread of
# the rounds (printed as min-max) as no change, and repeat a run that
# another process disturbed.
set -eu
[ $# -ge 2 ] || { sed -n '2,8p' "$0" >&2; exit 2; }
BASE=$1 HEAD=$2 ROUNDS=${3:-3}
ENGINES=${ENGINES:-moejs moejs-nocopy}
CELLS=${CELLS:-small:1:6 small:8:6 agent100k:1:6 agent100k:8:6 big8m:8:8 multi8x1m:8:8}
WARM=${WARM:-2}
RUN=${RUN:-}
REPO=$(cd "$(dirname "$0")/../.." && pwd)
SCN=$REPO/bench/testdata/scenarios
[ -f "$SCN/plugins.mjs" ] || { echo "plugin_ab.sh: $SCN is empty; run bench/scenarios/gen.py" >&2; exit 1; }
WORK=${WORK:-$(mktemp -d)}
mkdir -p "$WORK"
WORK=$(cd "$WORK" && pwd)

# tree <side> <rev or dir>: the directory of the side's moejs.
tree() {
	if [ -d "$2" ]; then
		(cd "$2" && pwd)
		return
	fi
	rm -rf "$WORK/$1-tree"
	mkdir -p "$WORK/$1-tree"
	git -C "$REPO" archive "$2" | tar -x -C "$WORK/$1-tree"
	echo "$WORK/$1-tree"
}

for side in base head; do
	if [ $side = base ]; then src=$BASE; else src=$HEAD; fi
	t=$(tree $side "$src")
	cp "$REPO/bench/go.mod" "$WORK/$side.mod"
	cp "$REPO/bench/go.sum" "$WORK/$side.sum"
	go mod edit -replace "github.com/MoeForge/moejs=$t" "$WORK/$side.mod"
	echo "building $side ($src)"
	(cd "$REPO/bench" && go build -mod=mod -modfile="$WORK/$side.mod" -ldflags=-funcalign=64 \
		-o "$WORK/pluginbench-$side" ./cmd/pluginbench)
done

echo "checking that both sides return the same results"
seen=" "
for cell in $CELLS; do
	s=${cell%%:*}
	case $seen in *" $s "*) continue ;; esac
	seen="$seen$s "
	for e in $ENGINES; do
		for side in base head; do
			"$WORK/pluginbench-$side" -dir "$SCN" -engine "$e" -scenario "$s" -check "$WORK/check-$side-$e-$s.txt"
		done
		if ! cmp -s "$WORK/check-base-$e-$s.txt" "$WORK/check-head-$e-$s.txt"; then
			echo "plugin_ab.sh: $e $s: the results differ, see $WORK/check-*-$e-$s.txt" >&2
			exit 1
		fi
	done
done

OUT=$WORK/ab.jsonl
: >"$OUT"
r=1
while [ $r -le "$ROUNDS" ]; do
	if [ $((r % 2)) = 1 ]; then order="base head"; else order="head base"; fi
	for cell in $CELLS; do
		s=${cell%%:*} rest=${cell#*:}
		w=${rest%%:*} d=${rest#*:}
		for e in $ENGINES; do
			for side in $order; do
				res=$($RUN "$WORK/pluginbench-$side" -dir "$SCN" -engine "$e" -scenario "$s" \
					-workers "$w" -warm "${WARM}s" -dur "${d}s" | tail -1)
				printf '{"round":%d,"side":"%s","result":%s}\n' "$r" "$side" "$res" >>"$OUT"
			done
		done
	done
	echo "round $r of $ROUNDS done"
	r=$((r + 1))
done

python3 - "$OUT" <<'EOF'
import json, statistics, sys
cells = {}
for line in open(sys.argv[1]):
    j = json.loads(line)
    r = j["result"]
    key = (r["engine"], r["scenario"], r["workers"])
    cells.setdefault(key, {"base": [], "head": []})[j["side"]].append(r)
print("%-13s %-10s %3s  %22s  %22s  %7s  %11s" % ("engine", "scenario", "w", "base req/s (min-max)", "head req/s (min-max)", "change", "GC base>head"))
for (e, s, w), sides in cells.items():
    b, h = sides["base"], sides["head"]
    if not b or not h:
        continue
    def cell(rs):
        v = [x["rps"] for x in rs]
        return statistics.median(v), "%.0f (%.0f-%.0f)" % (statistics.median(v), min(v), max(v))
    mb, tb = cell(b)
    mh, th = cell(h)
    gb = statistics.median(x["num_gc"] for x in b)
    gh = statistics.median(x["num_gc"] for x in h)
    print("%-13s %-10s %3d  %22s  %22s  %+6.1f%%  %5.0f>%-5.0f" % (e, s, w, tb, th, 100 * (mh / mb - 1), gb, gh))
EOF
echo "results in $OUT"
