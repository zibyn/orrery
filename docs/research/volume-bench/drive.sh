#!/bin/bash
# drive.sh: run the suite on each of $TARGETS; results to stdout.
#   local | volume-10s | volume-60s | juicefs | juicefs-wb
B=${BENCH_ROOT:-/var/tmp/orrery-bench}; S=$(cd "$(dirname "$0")" && pwd)
GR=${BENCH_GOROOT:-$(go env GOROOT)}
NODE=${BENCH_NODE:-$(readlink -f "$(command -v node)")}
bench() {
  docker run --rm --privileged --network host -v $B:/bench:rslave -v $S:/s:ro \
    -v "$(git -C "$S" rev-parse --show-toplevel)":/src:ro -v $GR:/goroot:ro -v "$(go env GOMODCACHE)":/gomod:ro \
    -v $NODE:/usr/local/bin/node:ro -e TMPDIR=/bench/tmp -e BENCH_PHASES="${BENCH_PHASES:-}" "$@"
}
for t in ${TARGETS:-local volume-10s volume-60s juicefs juicefs-wb}; do
  echo "##### $t $(date +%T)"
  case $t in
  local) bench orqestra-sandbox:q3pkgs bash -c 'rm -rf /bench/tmp; mkdir -p /bench/tmp/l; bash /s/bench.sh /bench/tmp/l' ;;
  volume-*) bench -e COMMIT=${t#volume-} -e ORRERY_BENCH_S3=http://127.0.0.1:9010 orqestra-sandbox:q3pkgs bash -c 'rm -rf /bench/tmp; mkdir -p /bench/tmp; cd /bench/tmp; ORRERY_BENCH_SCRIPT=/s/bench.sh ORRERY_BENCH_COMMIT=$COMMIT ORRERY_BENCH_S3=$ORRERY_BENCH_S3 /bench/bin/resourcefs-s3.test -test.run "^TestVolumeLocalBench\$" -test.v -test.timeout 4h 2>&1 | grep -v "^=== RUN"' ;;
  juicefs*)
    extra=; [ $t = juicefs-wb ] && extra=--writeback
    docker exec bench-redis redis-cli -p 6390 flushall >/dev/null
    docker run -d --name bench-jfs --privileged --network host -v $B:/bench -v $S:/s:ro -v $B/jfsmnt:/mnt/jfs:rshared --entrypoint /s/jfs.sh juicedata/mount:ce-v1.2.3 $extra >/dev/null
    until mount | grep -q 'jfsmnt type fuse.juicefs'; do sleep 1; done
    bench orqestra-sandbox:q3pkgs bash /s/bench.sh /bench/jfsmnt
    docker exec bench-jfs juicefs umount /mnt/jfs; docker rm -f bench-jfs >/dev/null ;;
  esac
done
echo "##### done $(date +%T)"
