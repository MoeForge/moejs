#!/bin/sh
# dispatchscreen.sh <engine test binary | disassembly of run> [<tree>]:
# the screen of the interpreter loop's dispatch, which gatebuild.sh runs on
# every engine binary it builds. It fails when (*Realm).run dispatches an
# instruction with more work than it should, or, given the checkout <tree>
# the binary was built from, when run has grown past the inliner's limit.
#
# Every instruction of every program runs the dispatch: the path from the
# loop's back edge, which each case's continue jumps to, to the jump
# through the table of the ops' cases. On amd64 with Go 1.25 to 1.27 it is
# twelve instructions: the move of the loop's pc into the register the
# instruction's word is read with (ipc), the store of the register window,
# the read of the word, pc+1 written back into the loop's register and
# stored to the frame's stack slot, the A operand decoded and stored, the
# op decoded and the jump. A change anywhere in run can move the register
# allocator's choices for the whole loop, and one instruction more there is
# +1 to +5 % instructions on every row, at no cost visible in the case that
# caused it. So the screen checks, in the disassembly:
#
#   - the path from the back edge to the table jump is straight;
#   - "pc leaves its register": pc+1 is written into the register the back
#     edge reads the loop's pc from. Written anywhere else, every back edge
#     moves it back, or the path does;
#   - "pc is spilled at every dispatch": the instruction's pc (ipc, or the
#     loop's pc before the increment) is not stored to the stack. It is
#     when any case uses ipc (or pc-1, pc-2, which fold to it) after a call
#     the case makes: a value live across any call in any case of run is
#     spilled where the loop defines it;
#   - no value is loaded from the stack on the path;
#   - the path has at most DISPATCH_MAX (default 12) instructions.
#
# With <tree>, it builds <tree>/engine with -gcflags=-m=2 and fails if the
# inliner considers run 'big': past 5000 IR nodes run inlines no callee
# costing more than 20, and helpers such as bitwiseInt32 become calls
# (Crypto +8 % instructions).
#
# Reading a failure: the screen prints the reason and the path, each line
# with its source position. For a spill or a pc out of its register, find
# the case that keeps ipc or pc across a call (a use after the call, often
# pc-2 or ipc to store the frame's pc again) and store or recompute it
# before the call instead; the register allocator's dump shows where the
# restore is:
#   go build -gcflags='-d=ssa/regalloc/dump=(*Realm).run' ./engine
# then look in the dump's file for the StoreReg of the loop's pc phi and
# the LoadReg of it. For a longer path with neither, compare the path with
# the base's (run the screen on both binaries): a value the loop carries in
# a register that a case now keeps across a call is spilled on the path
# too. For 'big', move cold code out of run (into coldOp or a helper). If
# the shape of the loop changes on purpose (a value it must carry, a new
# Go release), set DISPATCH_MAX to the new count and say why in the
# commit; SCREEN=0 in gatebuild.sh skips the screen, for a base built from
# a commit older than it.
#
# The disassembly form is for checking the screen itself: the output of
#   go tool objdump -s 'engine\.\(\*Realm\)\.run$' <engine test binary>
set -e
F=$1 T=$2
MAX=${DISPATCH_MAX:-12}
if [ "$(go env GOARCH)" != amd64 ]; then
	echo "dispatch screen: amd64 only, skipped"
	exit 0
fi
D=$(mktemp -d)
trap 'rm -rf "$D"' EXIT
if head -c 4 "$F" | grep -q '^TEXT'; then
	S=$F
else
	S=$D/run.s
	go tool objdump -s 'engine\.\(\*Realm\)\.run$' "$F" >"$S"
fi
awk -F '\t+' -v max="$MAX" '
function fail(why) {
	print "dispatch screen FAILED: " why
	if (start && end) {
		print "the path, from the back edge to the table jump:"
		for (k = start; k <= end; k++) print "\t" pos[k] "\t" ins[k]
	}
	bad = 1
	exit 1
}
# reg(s, k): the k-th comma-separated operand of the instruction s.
function reg(s, k,    p, n) {
	sub(/^[A-Z0-9]+ /, "", s)
	n = split(s, p, /, /)
	return k <= n ? p[k] : ""
}
/^TEXT / { next }
{
	a = ""; s = ""
	for (k = 1; k <= NF; k++) if (a == "" && $k ~ /^0x[0-9a-f]+$/) a = $k
	for (k = NF; k >= 1; k--) if ($k != "") { s = $k; break }
	if (a == "") next
	n++; adr[n] = a; ins[n] = s; pos[n] = $1
	sub(/^ +/, "", pos[n])
	if (s ~ /^JMP 0x[0-9a-f]+$/) jumps[substr(s, 5)]++
}
END {
	if (bad) exit 1
	if (!n) fail("no disassembly of (*Realm).run")
	# The back edge: the address most jumps go to, every continue.
	for (t in jumps) if (jumps[t] > most) { most = jumps[t]; latch = t }
	if (most < 20) fail("no back edge: no address is the target of 20 jumps")
	for (k = 1; k <= n; k++) if (adr[k] == latch) start = k
	if (!start) fail("the back edge " latch " starts no instruction")
	for (k = start; k <= n; k++) {
		if (ins[k] ~ /^JMP 0\([A-Z0-9]+\)\([A-Z0-9]+\*8\)$/) { end = k; break }
		if (ins[k] ~ /^J/ || ins[k] ~ /^(CALL|RET)/) { end = k; fail("the path branches before the table jump") }
	}
	if (!end) fail("no table jump after the back edge")
	for (k = start; k < end; k++) if (ins[k] ~ /\(SP\), [A-Z0-9]+$/ && ins[k] !~ /^LEAQ /) fail("a load from the stack on the path: " ins[k])
	# The read of the instruction word, indexed by ipc.
	for (k = start; k < end; k++) if (ins[k] ~ /^MOVL 0\([A-Z0-9]+\)\([A-Z0-9]+\*4\), /) { load = k; break }
	if (!load) fail("no read of the instruction word")
	ipc = ins[load]; sub(/^MOVL 0\([A-Z0-9]+\)\(/, "", ipc); sub(/\*4\).*/, "", ipc)
	# The loop pc: ipc itself, or the register moved into it.
	loop = ipc
	for (k = start; k < load; k++) if (ins[k] ~ /^MOVQ [A-Z0-9]+, [A-Z0-9]+$/ && reg(ins[k], 2) == ipc) loop = reg(ins[k], 1)
	for (k = load; k < end; k++) {
		if (ins[k] == "LEAQ 0x1(" ipc "), " reg(ins[k], 2)) { inc = k; next1 = reg(ins[k], 2); break }
		if (ins[k] == "INCQ " ipc || ins[k] == "ADDQ $0x1, " ipc) { inc = k; next1 = ipc; break }
	}
	if (!inc) fail("no pc+1 from the instruction pc " ipc)
	if (next1 != loop) fail("pc leaves its register: the back edge reads the loop pc from " loop ", the dispatch writes pc+1 to " next1)
	for (k = start; k < end; k++) {
		if (ins[k] ~ /^MOV[A-Z]* [A-Z0-9]+, .*\(SP\)$/) {
			src = reg(ins[k], 1)
			if (src == ipc || (src == loop && k < inc)) fail("pc is spilled at every dispatch: " ins[k])
		}
	}
	steps = end - start + 1
	if (steps > max) fail(steps " instructions on the path, more than " max " (DISPATCH_MAX)")
	print "dispatch screen: " steps " instructions, the loop pc in " loop ", the instruction pc in " ipc ": ok"
}
' "$S"
if [ -n "$T" ]; then
	(cd "$T/engine" && go build -gcflags=-m=2 -o /dev/null .) >"$D/m2.txt" 2>&1 || {
		cat "$D/m2.txt" >&2
		exit 1
	}
	if grep -q "function (\*Realm)\.run considered 'big'" "$D/m2.txt"; then
		echo "dispatch screen FAILED: the inliner considers (*Realm).run 'big' (more than 5000 IR nodes):"
		grep "function (\*Realm)\.run considered 'big'" "$D/m2.txt"
		exit 1
	fi
	echo "dispatch screen: (*Realm).run is not 'big' to the inliner: ok"
fi
