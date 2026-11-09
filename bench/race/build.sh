#!/usr/bin/env bash
# Builds the race runners into ${RACE_BIN:-race-bin}, all with Go 1.27.2,
# -trimpath and CGO_ENABLED=0:
#
#   suite-moejs, pluginrace-moejs  moejs from this checkout, -tags purego
#   suite, pluginrace              go-quickjs, goant, modernc, goja and Sobek
#   suite-old                      moejs v0.1.0-alpha.11, the previous release
#   suite-ctl, pluginrace-ctl      only with CTL_SRC: an off-screen control,
#                                  another moejs source tree, -tags purego
#
# The previous release is OLD_SRC, a checkout of v0.1.0-alpha.11, or else
# the tag in this repository. qjs and racehost (C QuickJS) are not built
# here; see README.md.
#
# Usage: bench/race/build.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
mkdir -p "${RACE_BIN:-race-bin}"
B=$(cd "${RACE_BIN:-race-bin}" && pwd)
export GOTOOLCHAIN=${GOTOOLCHAIN:-go1.27.2} CGO_ENABLED=0
go version | grep -q ' go1\.27\.2 ' || { echo "build.sh needs Go 1.27.2" >&2; exit 1; }
cd "$HERE"

GOFLAGS=-mod=readonly go build -trimpath -tags purego -o "$B/suite-moejs" ./cmd/suite
GOFLAGS=-mod=readonly go build -trimpath -tags purego -o "$B/pluginrace-moejs" ./cmd/pluginrace
GOFLAGS=-mod=readonly go build -trimpath -o "$B/suite" ./cmd/suite
GOFLAGS=-mod=readonly go build -trimpath -o "$B/pluginrace" ./cmd/pluginrace

# with_src NAME DIR go-build-args...: builds against another moejs tree
with_src() {
	local name=$1 src=$2
	shift 2
	cp go.mod "$B/.$name.mod"
	cp go.sum "$B/.$name.sum"
	go mod edit -modfile "$B/.$name.mod" -replace "github.com/MoeForge/moejs=$src"
	GOFLAGS=-mod=mod go build -trimpath -modfile "$B/.$name.mod" "$@"
}

OLD=${OLD_SRC:-}
if [ -z "$OLD" ]; then
	OLD=$B/.src-old
	rm -rf "$OLD"
	mkdir -p "$OLD"
	git -C "$ROOT" archive v0.1.0-alpha.11 | tar -x -C "$OLD"
fi
with_src old "$(cd "$OLD" && pwd)" -o "$B/suite-old" ./cmd/suite
if [ -n "${CTL_SRC:-}" ]; then
	CTL=$(cd "$CTL_SRC" && pwd)
	with_src ctl "$CTL" -tags purego -o "$B/suite-ctl" ./cmd/suite
	with_src ctl "$CTL" -tags purego -o "$B/pluginrace-ctl" ./cmd/pluginrace
fi

{
	go version
	for f in "$B"/suite* "$B"/pluginrace*; do
		echo "$(basename "$f"): $(sha256sum "$f" | cut -d' ' -f1)"
		go version -m "$f" | grep -E '^\s+(dep|=>)\s+(github.com/(MoeForge|dop251|grafana|go-quickjs|robomotionio)|modernc.org/quickjs)|-tags=|CGO_ENABLED' || true
	done
} > "$B/versions.txt"
echo "runners in $B"
