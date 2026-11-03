#!/bin/sh
# Self-test for the phase-grep gate (scripts/phase_grep.sh).
#
# Proves the gate actually fails when a Go source cites a plan phase: a
# probe file with a "// Phase 1" comment is planted in a dot-directory
# (invisible to the go tool, visible to grep -r), the gate must exit
# non-zero, and once the probe is removed the gate must pass again. The
# probe is removed on any exit so the working tree is never left holding
# the litter.
#
# Usage: scripts/phase_grep_test.sh   (from the repository root)

set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
probe_dir=$root/.phase-grep-selftest
probe=$probe_dir/probe.go

mkdir -p "$probe_dir"
trap 'rm -f "$probe"; rmdir "$probe_dir" 2>/dev/null || true' EXIT INT TERM
printf '// Phase 1 probe for the phase-grep self-test\npackage probe\n' >"$probe"

if "$root/scripts/phase_grep.sh" "$root" >/dev/null 2>&1; then
	echo "phase-grep self-test: gate passed with a planted Phase comment; the gate is inert" >&2
	exit 1
fi

# Without the probe the gate must pass: the repository itself is clean.
rm -f "$probe"
"$root/scripts/phase_grep.sh" "$root"

echo "phase-grep self-test: ok"
