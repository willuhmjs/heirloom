#!/bin/sh
set -e
mount -t nfsd nfsd /proc/fs/nfsd 2>/dev/null || true
echo '/export *(rw,fsid=0,sync,no_subtree_check,no_root_squash,insecure)' > /etc/exports
exportfs -rv
rpc.nfsd -N 3 -V 4 8
exec rpc.mountd -F -N 3 -V 4
