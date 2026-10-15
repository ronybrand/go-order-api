#!/usr/bin/env sh
# Fails when the total statement coverage of the profile is below the minimum (default 80).
# Usage: scripts/check-coverage.sh coverage.out [minimum]
set -eu
profile="${1:?usage: check-coverage.sh coverage.out [minimum]}"
minimum="${2:-80}"
total=$(go tool cover -func="$profile" | awk '/^total:/ { gsub("%", "", $3); print $3 }')
echo "total coverage: ${total}% (minimum ${minimum}%)"
awk -v total="$total" -v minimum="$minimum" 'BEGIN { exit (total + 0 >= minimum + 0) ? 0 : 1 }' \
  || { echo "coverage below the minimum" >&2; exit 1; }
