#!/bin/sh
# gatebuild.sh <tree> <outdir> <name>: the test binaries of the plugin-
# workload gate (docs/DESIGN.md §14.2) for the checkout <tree>, as
# <outdir>/{engine,root,bench}-<name>.test, for gateab.sh.
#
# They link with -funcalign=64. By default amd64 aligns functions to 32
# bytes, so a change anywhere before a hot function moves it by 32 within
# its 64-byte line, and that alone moves some rows by up to 10 % (docs/
# NOTES.md "LoopSum and the alignment of (*Realm).run"). With every
# function on a 64-byte boundary a function's placement depends on its own
# code only. The flag is the linker's, so any commit builds this way.
#
# PAD=<bytes> links a function of about that many bytes, never called, at
# the end of package bytecode, the first moejs package the linker lays
# out, so syntax, compiler, engine and all code linked after them move by
# PAD bytes against the Go runtime. Where the engine's hot code lands
# against the runtime's (the allocator, map lookups) decides how often
# their branches and lines collide in the CPU's branch target buffer and
# instruction cache: moving an unchanged engine alone moves front-end-bound
# rows such as PluginHook by 5-20 % (docs/NOTES.md "Code layout
# (2026-10)"). gateab.sh PADS=... runs its rounds over several such
# placements. The pad comes in through go build -overlay; the checkout is
# not touched.
#
# The engine binary then goes through dispatchscreen.sh, which fails the
# build when the interpreter loop's dispatch does more work than it should
# or (*Realm).run grows past the inliner's limit; its header says how to
# read a failure. SCREEN=0 skips it, for a base older than the screen.
set -e
T=$1 O=$2 N=$3
SD=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$O"
O=$(cd "$O" && pwd)
T=$(cd "$T" && pwd)
OV=
if [ -n "${PAD:-}" ]; then
	D=$(mktemp -d)
	trap 'rm -rf "$D"' EXIT
	printf 'package bytecode\n\nvar layoutPadOn = false\n\n// layoutPad is never called; its size sets where the code after it is linked.\nfunc layoutPad()\n\nfunc init() {\n\tif layoutPadOn {\n\t\tlayoutPad()\n\t}\n}\n' >"$D/pad.go"
	awk -v n="$PAD" 'BEGIN {
		print "#include \"textflag.h\"\n\nTEXT ·layoutPad(SB), NOSPLIT|NOFRAME, $0-0\n\tRET"
		for (i = 4; i < n; i += 4) print "\tLONG $0x90909090"
	}' >"$D/pad_amd64.s"
	printf '{"Replace":{"%s":"%s","%s":"%s"}}\n' \
		"$T/bytecode/zz_layoutpad.go" "$D/pad.go" \
		"$T/bytecode/zz_layoutpad_amd64.s" "$D/pad_amd64.s" >"$D/overlay.json"
	OV="-overlay=$D/overlay.json"
fi
cd "$T"
go test -c $OV -ldflags=-funcalign=64 -o "$O/engine-$N.test" ./engine
[ "${SCREEN:-1}" = 0 ] || sh "$SD/dispatchscreen.sh" "$O/engine-$N.test" "$T"
go test -c $OV -ldflags=-funcalign=64 -o "$O/root-$N.test" ./tests
cd bench && go test -c $OV -ldflags=-funcalign=64 -o "$O/bench-$N.test" .
