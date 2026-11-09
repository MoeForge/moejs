#!/usr/bin/env bash
# Downloads the V8 benchmark suite, version 7, at the mozilla/arewefastyet
# commit pinned in REVISION into ${MOEJS_V8_DIR:-$HOME/.cache/moejs/v8-v7}
# and checks every file against its sha256 in REVISION. Re-running it after
# REVISION changes replaces the files.
#
# Usage: bench/v8suite/fetch.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REV="$(awk 'NR == 1 { print $1 }' "$HERE/REVISION")"
DIR="${MOEJS_V8_DIR:-$HOME/.cache/moejs/v8-v7}"
SUMS="$(awk 'NR > 1 && $1 !~ /^#/ && NF == 2' "$HERE/REVISION")"

if [ -f "$DIR/REVISION" ] && [ "$(cat "$DIR/REVISION")" = "$REV" ] &&
	(cd "$DIR" && echo "$SUMS" | sha256sum --quiet -c - >/dev/null 2>&1); then
	echo "V8 suite $REV already in $DIR"
	exit 0
fi

mkdir -p "$DIR"
TMP="$(mktemp -d "$DIR/.fetch.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
URL="https://raw.githubusercontent.com/mozilla/arewefastyet/$REV/benchmarks/v8-v7"
echo "$SUMS" | while read -r _ f; do
	curl -fsSL -o "$TMP/$f" "$URL/$f"
done
(cd "$TMP" && echo "$SUMS" | sha256sum --quiet -c -)
echo "$SUMS" | while read -r _ f; do mv "$TMP/$f" "$DIR/$f"; done
echo "$REV" > "$DIR/REVISION"
echo "V8 suite $REV in $DIR"
