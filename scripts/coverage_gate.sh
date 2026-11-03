#!/bin/sh
# Coverage gate for Jevlar.
#
# Reads the [coverage] section of gates/thresholds.toml and enforces it
# against a `go test -coverprofile` profile:
#
#   - every internal package named in [coverage] that has statements must
#     meet its threshold;
#   - the "total" key is enforced across ./internal/... excluding
#     internal/testutil;
#   - packages with zero statements (skeleton doc.go packages) and entries
#     for packages that have no code yet are skipped — they have nothing to
#     measure.
#
# Usage: scripts/coverage_gate.sh [profile] [thresholds-file]

set -eu

profile=${1:-coverage.out}
gates=${2:-gates/thresholds.toml}

if [ ! -f "$profile" ]; then
	echo "coverage-gate: profile $profile not found (run the cover task first)" >&2
	exit 1
fi
if [ ! -f "$gates" ]; then
	echo "coverage-gate: $gates not found" >&2
	exit 1
fi

# Per-package statement coverage over internal packages, testutil excluded.
# Profile lines look like:
#   github.com/amscotti/jevlar/internal/config/env.go:20.51,22.2 2 1
# (file:range numStmts hitCount); a package's coverage is covered/total
# statements.
cov=$(awk '
	$1 !~ /\.go:/ { next }
	{
		pkg = $1
		sub(/:.*/, "", pkg)
		sub(/\/[^\/]*$/, "", pkg)
		if (pkg !~ /^github\.com\/amscotti\/jevlar\/internal\//) next
		if (pkg == "github.com/amscotti/jevlar/internal/testutil") next
		stmts = $2 + 0
		total[pkg] += stmts
		if (($3 + 0) > 0) covered[pkg] += stmts
	}
	END {
		for (p in total) printf "%s %.4f %d\n", p, covered[p] / total[p], total[p]
	}
' "$profile")

if [ -z "$cov" ]; then
	echo "coverage-gate: no internal statements in $profile; nothing to gate" >&2
	exit 1
fi

# Extract "key value" pairs from the [coverage] section.
thresholds=$(awk '
	/^\[coverage\]/ { insec = 1; next }
	/^\[/ { insec = 0 }
	insec {
		sub(/#.*/, "")
		eq = index($0, "=")
		if (eq == 0) next
		key = substr($0, 1, eq - 1); gsub(/[ \t]/, "", key)
		val = substr($0, eq + 1); gsub(/[ \t]/, "", val)
		if (key != "") print key, val
	}
' "$gates")

fail=0

# Package gates.
while read -r key threshold; do
	[ -n "$key" ] || continue
	if [ "$key" = "total" ]; then
		continue
	fi
	pkg="github.com/amscotti/jevlar/internal/$key"
	line=$(printf '%s\n' "$cov" | awk -v p="$pkg" '$1 == p { print }')
	if [ -z "$line" ]; then
		echo "coverage-gate: $pkg has no statements yet; skipping"
		continue
	fi
	actual=$(printf '%s\n' "$line" | awk '{ print $2 }')
	pct=$(printf '%.1f' "$(awk -v a="$actual" 'BEGIN { printf "%.1f", a * 100 }')")
	ok=$(awk -v a="$actual" -v t="$threshold" 'BEGIN { print (a >= t) ? "yes" : "no" }')
	if [ "$ok" = "yes" ]; then
		echo "coverage-gate: $pkg ${pct}% >= ${threshold}"
	else
		echo "coverage-gate: FAIL $pkg coverage ${pct}% < threshold ${threshold}" >&2
		fail=1
	fi
done <<EOF
$thresholds
EOF

# Total gate, weighted by statements across all measured internal packages.
total_actual=$(printf '%s\n' "$cov" | awk '{
	covered += $2 * $3
	total += $3
} END { if (total > 0) printf "%.4f", covered / total; else print "1.0000" }')
total_threshold=$(printf '%s\n' "$thresholds" | awk '$1 == "total" { print $2 }')
if [ -n "$total_threshold" ]; then
	total_pct=$(awk -v a="$total_actual" 'BEGIN { printf "%.1f", a * 100 }')
	ok=$(awk -v a="$total_actual" -v t="$total_threshold" 'BEGIN { print (a >= t) ? "yes" : "no" }')
	if [ "$ok" = "yes" ]; then
		echo "coverage-gate: total ${total_pct}% >= ${total_threshold}"
	else
		echo "coverage-gate: FAIL total coverage ${total_pct}% < threshold ${total_threshold}" >&2
		fail=1
	fi
fi

exit $fail
