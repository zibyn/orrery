#!/bin/bash
# bench.sh <dir>: run the workloads against <dir> (BENCH_PHASES narrows them).
set -uo pipefail
export PATH=/usr/local/bin:/goroot/bin:$PATH GOROOT=/goroot GOMODCACHE=/gomod GOPROXY=off GOTOOLCHAIN=local
export GOCACHE=$1/.gocache
git config --global --add safe.directory '*'
git config --global user.email bench@example.invalid; git config --global user.name bench
python3 "$(dirname "$0")/bench.py" "$1" ${BENCH_PHASES:-}
