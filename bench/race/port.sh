#!/usr/bin/env bash
# Builds the engine library (the module root package) of this checkout for
# every GOOS/GOARCH target of Go 1.27.2 with CGO_ENABLED=0, default and
# -tags purego, and lists the repository's assembly and C files (none) and
# the cgo and assembly files of the library's dependencies. The video's
# systems scene and methods page read $RACE_OUT/portability.
#
# Usage: bench/race/port.sh   (PORT_JOBS: parallel builds, default nproc)
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
mkdir -p "${RACE_OUT:-race-out}/portability"
P=$(cd "${RACE_OUT:-race-out}/portability" && pwd)
export GOTOOLCHAIN=${GOTOOLCHAIN:-go1.27.2} CGO_ENABLED=0 P
go version | grep -q ' go1\.27\.2 ' || { echo "port.sh needs Go 1.27.2" >&2; exit 1; }
cd "$ROOT"

go tool dist list > "$P/targets.txt"
git ls-files '*.s' '*.S' '*.c' '*.h' > "$P/native-files.txt"
go list -deps -f '{{if not .Standard}}{{.ImportPath}} cgo={{len .CgoFiles}} asm={{len .SFiles}}{{end}}' . > "$P/core-deps.txt"
go list -tags purego -deps -f '{{if not .Standard}}{{.ImportPath}} cgo={{len .CgoFiles}} asm={{len .SFiles}}{{end}}' . > "$P/purego-core-deps.txt"

one() {
	local tag=$1 target=$2 rc=0 log
	log=$P/$([ "$tag" = purego ] && echo purego-)core-${target%/*}-${target#*/}.log
	GOOS=${target%/*} GOARCH=${target#*/} timeout 900 go build $([ "$tag" = purego ] && echo -tags purego) -p 1 -o /dev/null . > "$log" 2>&1 || rc=$?
	printf '%s\t%s\t%s\n' "$tag" "$target" "$rc"
}
export -f one
{ sed 's/^/default /' "$P/targets.txt"; sed 's/^/purego /' "$P/targets.txt"; } |
	xargs -P "${PORT_JOBS:-$(nproc)}" -L 1 bash -c 'one "$0" "$1"' > "$P/all.tsv"
for t in default purego; do
	f=$([ "$t" = default ] && echo core.tsv || echo purego-core.tsv)
	while read -r tg; do
		awk -v a="$t" -v b="$tg" '$1 == a && $2 == b { print b "\t" $3 }' "$P/all.tsv"
	done < "$P/targets.txt" > "$P/$f"
done
echo "failed: $(awk '$3 != 0' "$P/all.tsv" | wc -l) of $(wc -l < "$P/all.tsv")"
