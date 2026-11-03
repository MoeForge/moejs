#!/bin/sh
# intl_webapi_bench.sh builds what bench/scripts/intl_webapi_bench.py runs
# into BIN (default /tmp/intl-webapi-bin) and runs it, appending to OUT
# (default docs/assets/intl-webapi-bench.jsonl); the rest of the arguments
# go to the Python script, for example the canaries:
#
#	NODE=~/opt/node-v24.22.0-linux-x64/bin/node sh bench/scripts/intl_webapi_bench.sh \
#		-canary 'cpu:248000:taskset -c 0-7 ./canary'
#
# The node:fs chart's rounds go to their own file:
#
#	OUT=docs/assets/nodefs-bench.jsonl NODE=... sh bench/scripts/intl_webapi_bench.sh \
#		-sections nodefs -canary ...
#
# ROUNDS (default 7) is the number of rounds to keep.
set -e
cd "$(dirname "$0")/../.."
BIN=${BIN:-/tmp/intl-webapi-bin}
OUT=${OUT:-docs/assets/intl-webapi-bench.jsonl}
mkdir -p "$BIN"
(cd bench && go test -c -o "$BIN/bench.test" . && go build -o "$BIN" ./cmd/webapibench ./cmd/intlfirst ./cmd/nodefsbench)
exec python3 bench/scripts/intl_webapi_bench.py -bin "$BIN" -node "${NODE:-node}" -o "$OUT" -rounds "${ROUNDS:-7}" "$@"
