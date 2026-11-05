#!/usr/bin/env bash
# Resets the replan Live demo to a known state before each recording take.
#   scripts/demo-reset.sh [demo-dir]     (default: ~/replan-demo)
# Needs: a built bin/replan, `replan login` done once (the re:Invent catalog
# requires registration), and Kiro CLI if you record the agent part.
set -euo pipefail
REPO="$(cd "$(dirname "$0")/.." && pwd)"
R="$REPO/bin/replan"
DIR="${1:-$HOME/replan-demo}"

go build -C "$REPO" -o bin/replan ./cmd/replan
case "$DIR" in "" | "/" | "$HOME" | "$HOME/") echo "refusing to reset $DIR" >&2; exit 1 ;; esac
# Empty the folder rather than deleting it, so terminal tabs already open in
# it stay valid between takes.
mkdir -p "$DIR" && cd "$DIR"
find . -mindepth 1 -maxdepth 1 -exec rm -rf {} +

cat > reinvent.yaml <<'YAML'
event: reinvent2026
rules: {maxPerDay: 4, arriveEarly: 10m}
sessions:
  - reserve: ARC308
  - reserve: COM317-R1
  - reserve: AMZ401-R
YAML

"$R" apply --sim --sim-reset --sim-open-seats --sim-full-rate 0 --auto-approve >/dev/null   # the week you booked
"$R" sim shake --sim --sim-open-seats -o shaken.json >/dev/null                             # the week happens
"$R" setup kiro --workspace --sim --sim-open-seats --sim-full-rate 0 \
  --catalog shaken.json --compare shaken-base.json --now "tue 12:40" >/dev/null

echo "Demo ready in $DIR (simulator; it is Tuesday 12:40 at re:Invent)."
echo "  cd $DIR && kiro-cli chat --agent replan --trust-workspace"
echo "  $R now --sim --sim-open-seats --catalog shaken.json --now 'tue 12:40' --at 'MGM Grand' --late 20m"
