#!/bin/sh
# Phase-grep gate for Jevlar.
#
# Comments in Go sources must never cite plan phases ("Phase 0",
# "Phase 1", ...): the sources stay reviewable without the plan document
# at hand, and the phase numbering is not part of the code's contract.
#
# The gate is an if/exit construct on purpose: `! grep ...` under
# `set -e` does not abort mid-script (the negation makes the pipeline a
# simple command whose status is inverted), so the gate was inert when
# composed with `&&` chains.
#
# Usage: scripts/phase_grep.sh [root]   (default: the repository root)

set -eu

root=${1:-.}

if grep -rnE 'Phase [0-9]' --include='*.go' "$root"; then
	echo "ci: Go sources must not cite plan phases" >&2
	exit 1
fi
