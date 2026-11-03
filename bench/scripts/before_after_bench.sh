#!/bin/sh
# before_after_bench.sh builds what bench/scripts/before_after_bench.py runs
# from two revisions of this repository, BEFORE and AFTER, into BIN/before
# and BIN/after (default /tmp/before-after) and runs it, appending to OUT
# (default /tmp/before-after.jsonl); the rest of the arguments go to the
# Python script:
#
#	sh bench/scripts/before_after_bench.sh REV1 REV2 -before-date 2026-10-24 -after-date 2026-10-25 \
#		-canary 'cpu:248000:taskset -c 0-7 ./canary'
#
# A revision without webapi/global_bench_test.go gets this tree's copy.
# ROUNDS (default 7) is the number of rounds to keep.
set -e
cd "$(dirname "$0")/../.."
BIN=${BIN:-/tmp/before-after}
OUT=${OUT:-/tmp/before-after.jsonl}
for side in before after; do
	rm -rf "$BIN/$side-tree"
	mkdir -p "$BIN/$side-tree" "$BIN/$side"
	git archive "$1" | tar -x -C "$BIN/$side-tree"
	shift
	[ -e "$BIN/$side-tree/webapi/global_bench_test.go" ] || cp webapi/global_bench_test.go "$BIN/$side-tree/webapi/"
	(cd "$BIN/$side-tree/bench" && go test -c -o "$BIN/$side/bench.test" . && go build -o "$BIN/$side" ./cmd/webapibench)
	(cd "$BIN/$side-tree" && go test -c -o "$BIN/$side/webapi.test" ./webapi)
done
exec python3 bench/scripts/before_after_bench.py -before "$BIN/before" -after "$BIN/after" -o "$OUT" -rounds "${ROUNDS:-7}" "$@"
