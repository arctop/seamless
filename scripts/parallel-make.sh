#!/bin/sh
# parallel-make: run make targets at the same time, then report each one in
# the order given -- its output under a header with its time -- and fail if
# any failed, naming them.
#
# Why this exists: `make check` ran its static steps (build, vet, cross-check,
# lint, vulncheck, docs-check, ...) one after another, about 45 seconds on a
# 24-core Mac of which the slowest single step was about 12. None of them reads
# what another writes, so they run together and the gate waits for the slowest
# instead of the sum. The cost of running them at once is that a red step no
# longer stops the rest: every red step is reported, not just the first, and
# that report comes after the slowest step rather than at the first failure.
#
# Xcode's make is GNU make 3.81, which has no --output-sync, so `make -j`
# would interleave the steps' output line by line: each target writes to its
# own file here instead and is printed whole once all of them have ended.
#
# Usage: scripts/parallel-make.sh <target>...
#   MAKE names the make to run (the Makefile passes its own $(MAKE)); a
#   command-line variable such as GO=... reaches every target through
#   MAKEFLAGS, as it would through a recursive $(MAKE).
set -u

MAKE=${MAKE:-make}

tmp=$(mktemp -d) || exit 1
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT TERM

i=0
for t in "$@"; do
	i=$((i + 1))
	(
		start=$(date +%s)
		$MAKE --no-print-directory "$t" >"$tmp/$i.out" 2>&1
		echo $? >"$tmp/$i.rc"
		echo $(($(date +%s) - start)) >"$tmp/$i.secs"
	) &
done
wait

failed=''
i=0
for t in "$@"; do
	i=$((i + 1))
	rc=$(cat "$tmp/$i.rc" 2>/dev/null || echo 1)
	if [ "$rc" = 0 ]; then
		echo "==> $t ($(cat "$tmp/$i.secs")s)"
	else
		echo "==> $t FAILED ($(cat "$tmp/$i.secs" 2>/dev/null || echo ?)s)"
		failed="$failed $t"
	fi
	cat "$tmp/$i.out"
done
if [ -n "$failed" ]; then
	echo "FAIL:$failed" >&2
	exit 1
fi
