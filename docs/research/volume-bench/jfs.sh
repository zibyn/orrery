#!/bin/bash
# jfs.sh <extra mount flags>: format a fresh JuiceFS volume and mount it at /mnt/jfs in the foreground.
set -e
name=b$(date +%s)
juicefs format --storage s3 --bucket http://127.0.0.1:9010/$name --access-key bench --secret-key benchbench redis://127.0.0.1:6390/0 $name >/dev/null 2>&1
rm -rf /bench/jfscache
exec juicefs mount --cache-dir /bench/jfscache "$@" redis://127.0.0.1:6390/0 /mnt/jfs
