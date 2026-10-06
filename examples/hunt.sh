#!/bin/sh
# Hunts for bugs in one controller with bin/reconciler-fuzzer: each sequence
# family in a directory, then drawn seeds, each in an invocation of its own,
# until the time box runs out. Every invocation writes under the output
# directory, where a failing run keeps its evidence. HUNT_MINUTES is the time
# box. HUNT_RUNS seeds are drawn from HUNT_SEED on.
set -eu
[ "$#" -eq 3 ] || {
  echo "Usage: examples/hunt.sh <target.yaml> <families directory> <output directory>" >&2
  exit 2
}
: "${HUNT_MINUTES:?sets the time box}" "${HUNT_RUNS:?sets how many seeds to draw}" "${HUNT_SEED:?sets the first seed}"

target=$1
families=$2
out=$3
# A rebuild of bin/reconciler-fuzzer during the hunt changes nothing.
fuzzer=$out/reconciler-fuzzer
mkdir -p "$out"
cp bin/reconciler-fuzzer "$fuzzer"
end=$(($(date +%s) + HUNT_MINUTES * 60))
passed=0
failed=

# hunt runs reconciler-fuzzer into $out/$1 on what the time box has left. It
# fails once the box is empty.
hunt() {
  name=$1
  shift
  left=$((end - $(date +%s)))
  if [ "$left" -le 0 ]; then
    echo "the time box ran out before $name."
    return 1
  fi
  echo "==> $out/$name"
  status=0
  "$fuzzer" run --target "$target" --deadline "${left}s" --out "$out/$name" "$@" || status=$?
  # reconciler-fuzzer exits 2 when its deadline, the box's end, stops it.
  if [ "$status" -eq 2 ] && [ "$(date +%s)" -ge "$end" ]; then
    echo "the time box ran out during $name."
    return 1
  fi
  case $status in
    0) passed=$((passed + 1)) ;;
    1 | 2) failed="$failed
  $out/$name exited $status" ;;
    # reconciler-fuzzer died of a signal.
    *) exit "$status" ;;
  esac
}

all() {
  for family in "$families"/*.json; do
    [ -e "$family" ] || continue
    hunt "$(basename "$family" .json)" "$family" || return 0
  done
  # The first seed's invocation also runs the baseline, which every seed shares.
  i=0
  baseline=
  while [ "$i" -lt "$HUNT_RUNS" ]; do
    hunt "seed-$((HUNT_SEED + i))" --seed "$((HUNT_SEED + i))" --runs 1 $baseline || return 0
    baseline=--no-baseline
    i=$((i + 1))
  done
}

all
echo "$passed passed."
if [ -n "$failed" ]; then
  echo "These did not pass. Each is a candidate to triage, not yet a bug:$failed"
  exit 1
fi
