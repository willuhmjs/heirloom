#!/usr/bin/env bash
# End-to-end test: a throwaway kind cluster, a real NFS server and the real chart.
#
#   scripts/e2e.sh                 run everything, delete the cluster afterwards
#   KEEP=1 scripts/e2e.sh          leave the cluster and NFS server running to poke at
#   CLUSTER=foo scripts/e2e.sh     use another cluster name
#
# Needs: docker, kind, kubectl, helm. Takes about five minutes the first time (image pulls).
# The NFS server needs the nfsd kernel module in the Docker VM / host kernel.
set -euo pipefail
cd "$(dirname "$0")/.."

CLUSTER=${CLUSTER:-backshots-e2e}
IMAGE=backshots:e2e
NFS=$CLUSTER-nfs
NS=demo
export PATH="$HOME/go/bin:$PATH"

# A private kubeconfig: the test never reads or changes ~/.kube/config, and nothing that manages that file can
# rename or remove the test cluster's context underneath it. Every tool below inherits it through KUBECONFIG.
KUBECONFIG=$(mktemp -t backshots-e2e-kubeconfig.XXXXXX)
export KUBECONFIG

k() { kubectl "$@"; }
nfs() { docker exec "$NFS" "$@"; }

pass=0
fail=0
ok() { echo "  ok    $*"; pass=$((pass + 1)); }
bad() { echo "  FAIL  $*"; fail=$((fail + 1)); }
step() { printf '\n== %s\n' "$*"; }

# check DESCRIPTION COMMAND...   passes when the command succeeds
check() {
  local desc=$1
  shift
  if "$@" >/dev/null 2>&1; then ok "$desc"; else bad "$desc"; fi
}
# wait_for DESCRIPTION SECONDS COMMAND...   passes when the command succeeds within the time
wait_for() {
  local desc=$1 secs=$2
  shift 2
  local end=$((SECONDS + secs))
  while ((SECONDS < end)); do
    if "$@" >/dev/null 2>&1; then
      ok "$desc"
      return
    fi
    sleep 2
  done
  bad "$desc (not true after ${secs}s)"
}
# equals DESCRIPTION EXPECTED COMMAND...   passes when the command prints EXPECTED
equals() {
  local desc=$1 want=$2
  shift 2
  local got
  got=$("$@" 2>&1 | tr -d '\r' | head -1 || true)
  if [[ "$got" == "$want" ]]; then ok "$desc"; else bad "$desc (got '$got', want '$want')"; fi
}
# rejected DESCRIPTION YAML   passes when the API server refuses the manifest
rejected() {
  if k apply --dry-run=server -f - <<<"$2" >/dev/null 2>&1; then bad "$1 (was accepted)"; else ok "$1"; fi
}

cleanup() {
  if [[ -z "${KEEP:-}" ]]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    rm -f "$KUBECONFIG"
    docker rm -f "$NFS" >/dev/null 2>&1 || true
    docker volume rm "$NFS-data" >/dev/null 2>&1 || true
  else
    echo
    echo "KEEP=1: cluster '$CLUSTER' and NFS container '$NFS' left running."
    echo "  export KUBECONFIG=$KUBECONFIG"
    echo "  kubectl -n $NS get bp"
    echo "  kind delete cluster --name $CLUSTER; docker rm -f $NFS; docker volume rm $NFS-data; rm $KUBECONFIG"
  fi
}
trap cleanup EXIT

phase() { k -n $NS get restore "$1" -o jsonpath='{.status.phase}'; }
restore_done() { [[ "$(phase "$1")" == Succeeded || "$(phase "$1")" == Failed ]]; }
pg() { k -n $NS exec deploy/postgres -- psql -U postgres "$@"; }
web() { k -n $NS exec deploy/web -- sh -c "$1"; }

step "Cluster, NFS server, operator"
docker build -q -t "$IMAGE" . >/dev/null
kind create cluster --name "$CLUSTER" --wait 120s >/dev/null
kind load docker-image "$IMAGE" --name "$CLUSTER" >/dev/null
docker build -q -t "$NFS" scripts/e2e/nfs >/dev/null
docker volume create "$NFS-data" >/dev/null
docker rm -f "$NFS" >/dev/null 2>&1 || true
docker run -d --name "$NFS" --privileged --network kind -v "$NFS-data":/export "$NFS" >/dev/null
NFS_IP=$(docker inspect "$NFS" --format '{{(index .NetworkSettings.Networks "kind").IPAddress}}')
wait_for "NFS server accepts a mount from the kind node" 40 \
  docker exec "$CLUSTER-control-plane" sh -c "mkdir -p /mnt/t && mount -t nfs4 -o proto=tcp $NFS_IP:/ /mnt/t && umount /mnt/t"
helm install backshots charts/backshots -n backshots-system --create-namespace \
  --set image.repository=backshots --set image.tag=e2e --set image.pullPolicy=IfNotPresent >/dev/null
wait_for "operator is ready" 120 k -n backshots-system rollout status deploy/backshots --timeout=5s

step "Admission rejects bad specs"
rejected "source type without its block" 'apiVersion: backshots.io/v1alpha1
kind: BackupPolicy
metadata: {name: bad, namespace: default}
spec: {storeRef: nas, schedule: "@hourly", source: {type: files}}'
rejected "two source blocks" 'apiVersion: backshots.io/v1alpha1
kind: BackupPolicy
metadata: {name: bad, namespace: default}
spec: {storeRef: nas, schedule: "@hourly", source: {type: files, files: {pvc: a}, sqlite: {pvc: b, paths: [x.db]}}}'
rejected "restore from: yesterday" 'apiVersion: backshots.io/v1alpha1
kind: Restore
metadata: {name: bad, namespace: default}
spec: {policyRef: x, from: yesterday}'
rejected "all-zero retention" 'apiVersion: backshots.io/v1alpha1
kind: BackupStore
metadata: {name: bad}
spec: {nfs: {server: 1.2.3.4, path: /x}, retention: {}}'
rejected "relative NFS path" 'apiVersion: backshots.io/v1alpha1
kind: BackupStore
metadata: {name: bad}
spec: {nfs: {server: 1.2.3.4, path: volume1/x}}'

step "Workloads and data"
k create ns $NS >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: backshots.io/v1alpha1
kind: BackupStore
metadata: {name: nas}
spec: {nfs: {server: $NFS_IP, path: /}, retention: {hourly: 3, daily: 2}}
---
apiVersion: v1
kind: Secret
metadata: {name: pg, namespace: $NS}
stringData: {password: s3cret}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: files-data, namespace: $NS}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 1Gi}}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: sqlite-data, namespace: $NS}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 1Gi}}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: pg-data, namespace: $NS}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 1Gi}}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: postgres, namespace: $NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: postgres}}
  template:
    metadata: {labels: {app: postgres}}
    spec:
      containers:
      - name: postgres
        image: postgres:16
        env:
        - {name: POSTGRES_PASSWORD, valueFrom: {secretKeyRef: {name: pg, key: password}}}
        - {name: PGDATA, value: /var/lib/postgresql/data/pgdata}
        volumeMounts: [{name: d, mountPath: /var/lib/postgresql/data}]
      volumes: [{name: d, persistentVolumeClaim: {claimName: pg-data}}]
---
apiVersion: v1
kind: Service
metadata: {name: postgres, namespace: $NS}
spec: {selector: {app: postgres}, ports: [{port: 5432}]}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: $NS}
spec:
  replicas: 1
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      containers:
      - name: web
        image: $IMAGE
        imagePullPolicy: IfNotPresent
        command: [sh, -c, "sleep 1000000"]
        volumeMounts: [{name: f, mountPath: /data}, {name: s, mountPath: /db}]
      volumes:
      - {name: f, persistentVolumeClaim: {claimName: files-data}}
      - {name: s, persistentVolumeClaim: {claimName: sqlite-data}}
EOF
# Over TCP on purpose: the image first runs a temporary server that listens on the Unix socket only, answers
# pg_isready, then shuts down and restarts as the real one.
wait_for "postgres is accepting connections" 240 k -n $NS exec deploy/postgres -- pg_isready -h 127.0.0.1 -U postgres
wait_for "web is running" 120 k -n $NS rollout status deploy/web --timeout=5s
pg -qc "create database app" -c "create database blog" >/dev/null
pg -d app -qc "create table users(id int, n text); insert into users select g,'u'||g from generate_series(1,500) g;" >/dev/null
pg -d blog -qc "create table posts(id int); insert into posts select generate_series(1,20);" >/dev/null
# big.bin (64 MiB of random bytes, so it neither dedups away nor compresses) is the dedup probe: a later
# step overwrites a few bytes of it and checks what the second backup actually added to the repository.
web 'mkdir -p /data/docs/deep /data/media && cd /data && echo hello > docs/a.txt && echo world > docs/deep/b.txt &&
  dd if=/dev/urandom of=media/blob.bin bs=1k count=256 2>/dev/null && dd if=/dev/urandom of=media/big.bin bs=1M count=64 2>/dev/null &&
  printf "#!/bin/sh\necho hi\n" > run.sh &&
  chmod 750 run.sh && chown 1000:1000 docs/a.txt run.sh && ln -s docs/a.txt link-to-a &&
  sqlite3 /db/app.db "pragma journal_mode=wal; create table t(id integer primary key, v text); insert into t(v) select value from generate_series(1,300);"' >/dev/null

k apply -f - >/dev/null <<EOF
apiVersion: backshots.io/v1alpha1
kind: BackupPolicy
metadata: {name: files, namespace: $NS}
spec:
  storeRef: nas
  schedule: "@hourly"
  source: {type: files, files: {pvc: files-data}}
  trigger: {manual: "1"}
---
apiVersion: backshots.io/v1alpha1
kind: BackupPolicy
metadata: {name: keeper, namespace: $NS}
spec:
  storeRef: nas
  schedule: "@hourly"
  source: {type: files, files: {pvc: files-data}}
  retention: {weekly: 1}
  trigger: {manual: "1"}
---
apiVersion: backshots.io/v1alpha1
kind: BackupPolicy
metadata: {name: sqlite, namespace: $NS}
spec:
  storeRef: nas
  schedule: "@hourly"
  source: {type: sqlite, sqlite: {pvc: sqlite-data, paths: [app.db]}}
  verify: {schedule: "@daily"}
  trigger: {manual: "1"}
---
apiVersion: backshots.io/v1alpha1
kind: BackupPolicy
metadata: {name: pg, namespace: $NS}
spec:
  storeRef: nas
  schedule: "@hourly"
  source:
    type: postgres
    postgres:
      image: postgres:16
      host: postgres.$NS.svc
      passwordSecretRef: {name: pg, key: password}
  verify: {schedule: "@daily", database: app, sql: ["select count(*) = 500 from users"]}
  trigger: {manual: "1"}
---
apiVersion: backshots.io/v1alpha1
kind: BackupPolicy
metadata: {name: pg-ambiguous, namespace: $NS}
spec:
  storeRef: nas
  schedule: "@hourly"
  source:
    type: postgres
    postgres:
      image: postgres:16
      host: postgres.$NS.svc
      passwordSecretRef: {name: pg, key: password}
  verify: {schedule: "@daily", sql: ["select true"]}
EOF

step "Backups"
for p in files keeper sqlite pg; do
  wait_for "$p: first backup succeeded and is Healthy" 150 \
    bash -c "[[ \$(kubectl -n $NS get bp $p -o jsonpath='{.status.conditions[?(@.type==\"Healthy\")].status}') == True ]]"
done
equals "a verify policy that names no database is rejected by the controller" "InvalidSpec" \
  k -n $NS get bp pg-ambiguous -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'
check "status.lastSnapshot.name is a restic short id" \
  bash -c "[[ \$(kubectl -n $NS get bp files -o jsonpath='{.status.lastSnapshot.name}') =~ ^[0-9a-f]{8}\$ ]]"
check "the Healthy message is the restic run summary" bash -c \
  "kubectl -n $NS get bp files -o jsonpath='{.status.conditions[?(@.type==\"Healthy\")].message}' | grep -Eq 'restic snapshot [0-9a-f]{8}: [0-9]+ files, .* added to repository; kept [0-9]+, pruned [0-9]+'"
check "each policy folder on the export holds a restic repository" nfs sh -c \
  "for p in files keeper pg sqlite; do test -f /export/$NS/\$p/restic/config || exit 1; done"
check "...and holds nothing else (no plain snapshot folders)" nfs sh -c \
  "for p in files keeper pg sqlite; do [ \"\$(ls /export/$NS/\$p)\" = restic ] || exit 1; done"
check "no timestamp-named snapshot dirs anywhere on the export" nfs sh -c \
  "! ls /export/$NS/*/ | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T'"

step "Dedup: a changed file costs its changes, not its size"
size_before=$(nfs du -sk /export/$NS/files/restic | cut -f1 | tr -dc 0-9)
web 'dd if=/dev/zero bs=1 count=4 seek=1048576 conv=notrunc of=/data/media/big.bin 2>/dev/null' >/dev/null
sha_big=$(web 'sha256sum /data/media/big.bin | cut -d" " -f1')
k -n $NS patch bp files --type merge -p '{"spec":{"trigger":{"manual":"2"}}}' >/dev/null
wait_for "second files backup finished" 180 \
  bash -c "[[ \$(kubectl -n $NS get bp files -o jsonpath='{.status.lastManualSync}') == 2 ]]"
size_after=$(nfs du -sk /export/$NS/files/restic | cut -f1 | tr -dc 0-9)
# Content-defined chunking re-stores at most one max-size chunk (8 MiB) plus small metadata for a 4-byte
# edit, so the repository must grow by well under a fifth of the 64 MiB file; a scheme that copied the
# whole file into every snapshot would have doubled it.
check "re-backup grew the restic repository by under 20% of the 64 MiB file" \
  bash -c "[[ $((size_after - size_before)) -le $((64 * 1024 * 20 / 100)) ]]"
added=$(k -n $NS get bp files -o jsonpath='{.status.conditions[?(@.type=="Healthy")].message}' | awk '
  { for (i = 1; i < NF; i++) if ($i == "added" && $(i+1) == "to") {
      u = $(i-1)
      m = (u == "B" ? 1 : (u == "KiB" ? 1024 : (u == "MiB" ? 1048576 : (u == "GiB" ? 1073741824 : 0))))
      printf "%d", $(i-2) * m } }')
check "...and the run message agrees: little was added to the repository" \
  bash -c "[[ $added -lt $((64 * 1024 * 1024 * 20 / 100)) ]]"

step "Retention prunes and keeps what it should"
for v in 2 3; do
  k -n $NS patch bp keeper --type merge -p "{\"spec\":{\"trigger\":{\"manual\":\"$v\"}}}" >/dev/null
  if [[ $v == 2 ]]; then
    wait_for "the mover pod mounts only its policy's slice of the export, plus /scratch" 30 bash -c \
      "mounts=\$(kubectl -n $NS get pods -l backshots.io/policy=keeper -o jsonpath='{.items[0].spec.containers[?(@.name==\"mover\")].volumeMounts}') &&
       echo \"\$mounts\" | grep -q '\"mountPath\":\"/store\"' && echo \"\$mounts\" | grep -q '\"subPath\":\"$NS/keeper\"' &&
       echo \"\$mounts\" | grep -q '\"mountPath\":\"/scratch\"'"
  fi
  wait_for "keeper backup $v finished" 120 \
    bash -c "[[ \$(kubectl -n $NS get bp keeper -o jsonpath='{.status.lastManualSync}') == $v ]]"
done
equals "retention {weekly: 1} keeps exactly one snapshot" "1" \
  k -n $NS get bp keeper -o jsonpath='{.status.snapshotsKept}'

step "Verify restores the snapshot and runs the assertions"
k -n $NS patch bp pg --subresource=status --type merge -p '{"status":{"lastVerifyScheduleTime":"2020-01-01T00:00:00Z"}}' >/dev/null
k -n $NS patch bp sqlite --subresource=status --type merge -p '{"status":{"lastVerifyScheduleTime":"2020-01-01T00:00:00Z"}}' >/dev/null
wait_for "postgres verify passed" 180 \
  bash -c "[[ \$(kubectl -n $NS get bp pg -o jsonpath='{.status.lastVerify.ok}') == true ]]"
wait_for "sqlite verify passed" 120 \
  bash -c "[[ \$(kubectl -n $NS get bp sqlite -o jsonpath='{.status.lastVerify.ok}') == true ]]"
check "the postgres verify message says check ok" bash -c \
  "kubectl -n $NS get bp pg -o jsonpath='{.status.lastVerify.message}' | grep -q 'check ok'"
check "the sqlite verify message says check ok" bash -c \
  "kubectl -n $NS get bp sqlite -o jsonpath='{.status.lastVerify.message}' | grep -q 'check ok'"

step "Restore: postgres"
pg -d app -qc "drop table users" >/dev/null
pg -qc "drop database blog" >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: backshots.io/v1alpha1
kind: Restore
metadata: {name: pg-restore, namespace: $NS}
spec: {policyRef: pg, scaleDown: [{kind: Deployment, name: web}]}
EOF
wait_for "restore finished" 180 restore_done pg-restore
equals "restore succeeded" "Succeeded" phase pg-restore
equals "the dropped table is back" "500" pg -d app -tAc "select count(*) from users"
equals "the dropped database is back" "20" pg -d blog -tAc "select count(*) from posts"
equals "the scaled-down workload came back" "1" k -n $NS get deploy web -o jsonpath='{.spec.replicas}'
equals "the Flux annotation was removed" "" k -n $NS get deploy web -o jsonpath='{.metadata.annotations.kustomize\.toolkit\.fluxcd\.io/reconcile}'
check "a Succeeded event was recorded" bash -c "kubectl -n $NS get events --field-selector involvedObject.name=pg-restore | grep -q Succeeded"

step "Restore: files (one path) and sqlite"
web 'rm -rf /data/docs; echo broken > /data/run.sh' >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: backshots.io/v1alpha1
kind: Restore
metadata: {name: files-restore, namespace: $NS}
spec: {policyRef: files, paths: [docs]}
---
apiVersion: backshots.io/v1alpha1
kind: Restore
metadata: {name: sqlite-restore, namespace: $NS}
spec: {policyRef: sqlite}
EOF
web 'sqlite3 /db/app.db "delete from t"; echo junk > /db/app.db-wal; echo junk > /db/app.db-journal' >/dev/null
wait_for "files restore finished" 120 restore_done files-restore
wait_for "sqlite restore finished" 120 restore_done sqlite-restore
equals "the restored path is back" "hello" web 'cat /data/docs/a.txt'
equals "the restored file keeps its owner" "1000" web 'stat -c %u /data/docs/a.txt'
equals "a file outside the restored path was left alone" "broken" web 'cat /data/run.sh'
equals "sqlite rows are back" "300" web 'sqlite3 /db/app.db "select count(*) from t"'
equals "sqlite integrity is ok" "ok" web 'sqlite3 /db/app.db "pragma integrity_check"'
equals "stale WAL and journal files are gone" "0" web 'ls /db | grep -c -e -wal -e -journal || true'

step "Restore: delete semantics, from a snapshot id"
snap=$(k -n $NS get bp files -o jsonpath='{.status.lastSnapshot.name}')
web 'echo extra > /data/extra.txt && rm -f /data/link-to-a' >/dev/null
k apply -f - >/dev/null <<EOF
apiVersion: backshots.io/v1alpha1
kind: Restore
metadata: {name: files-delete, namespace: $NS}
spec: {policyRef: files, from: $snap, delete: true, scaleDown: [{kind: Deployment, name: web}]}
EOF
wait_for "restore by id finished" 180 restore_done files-delete
equals "restore succeeded" "Succeeded" phase files-delete
equals "...resolving the restic id it was given" "$snap" k -n $NS get restore files-delete -o jsonpath='{.status.snapshot}'
wait_for "web is back" 120 k -n $NS rollout status deploy/web --timeout=5s
check "delete removed the file written after the snapshot" web 'test ! -e /data/extra.txt'
check "the backed-up symlink is restored" web 'test -L /data/link-to-a'
equals "restored files keep owner and mode" "1000:1000:750" web 'stat -c %u:%g:%a /data/run.sh'
equals "the file broken after the snapshot holds snapshot content again" "#!/bin/sh" web 'head -1 /data/run.sh'
equals "the 64 MiB file round-trips byte for byte" "$sha_big" web 'sha256sum /data/media/big.bin | cut -d" " -f1'

step "Restore safety"
k apply -f - >/dev/null <<EOF
apiVersion: backshots.io/v1alpha1
kind: Restore
metadata: {name: bad-combo, namespace: $NS}
spec: {policyRef: files, paths: [docs], delete: true}
EOF
wait_for "a restore combining delete with paths fails before touching anything" 60 \
  bash -c "[[ \$(kubectl -n $NS get restore bad-combo -o jsonpath='{.status.phase}') == Failed ]]"
check "...and says why" bash -c \
  "kubectl -n $NS get restore bad-combo -o jsonpath='{.status.message}' | grep -q 'not both'"

k apply -f - >/dev/null <<EOF
apiVersion: backshots.io/v1alpha1
kind: Restore
metadata: {name: bad-pvc, namespace: $NS}
spec: {policyRef: files, targetPVC: nope, scaleDown: [{kind: Deployment, name: web}]}
EOF
wait_for "a restore into a missing PVC fails" 60 bash -c "[[ \$(kubectl -n $NS get restore bad-pvc -o jsonpath='{.status.phase}') == Failed ]]"
equals "...and the workload was never stopped" "1" k -n $NS get deploy web -o jsonpath='{.spec.replicas}'

k apply -f - >/dev/null <<EOF
apiVersion: backshots.io/v1alpha1
kind: Restore
metadata: {name: q1, namespace: $NS}
spec: {policyRef: files, paths: [docs], scaleDown: [{kind: Deployment, name: web}]}
EOF
sleep 2
k apply -f - >/dev/null <<EOF
apiVersion: backshots.io/v1alpha1
kind: Restore
metadata: {name: q2, namespace: $NS}
spec: {policyRef: files, paths: [docs]}
EOF
wait_for "the second restore of the policy waits for the first" 60 \
  bash -c "kubectl -n $NS get restore q2 -o jsonpath='{.status.message}' | grep -q 'waiting for restore'"
equals "the policy pauses scheduling while a restore runs" "RestoreInProgress" \
  k -n $NS get bp files -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'
wait_for "the first restore scaled the workload down" 60 bash -c "[[ \$(kubectl -n $NS get deploy web -o jsonpath='{.spec.replicas}') == 0 ]]"
k -n $NS delete restore q1 --wait=true --timeout=90s >/dev/null 2>&1 || true
equals "deleting a restore mid-flight puts the workload back" "1" k -n $NS get deploy web -o jsonpath='{.spec.replicas}'
wait_for "the queued restore then runs to completion" 120 restore_done q2
equals "...and succeeds" "Succeeded" phase q2
equals "scheduling resumes afterwards" "Scheduled" \
  k -n $NS get bp files -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'

step "Metrics are authenticated"
POD_IP=$(k -n backshots-system get pod -l app.kubernetes.io/name=backshots -o jsonpath='{.items[0].status.podIP}')
node() { docker exec "$CLUSTER-control-plane" "$@"; }
equals "anonymous request is refused (401)" "401" node curl -sk -o /dev/null -w '%{http_code}' "https://$POD_IP:8443/metrics"
k create sa scraper -n $NS >/dev/null
TOKEN=$(k -n $NS create token scraper)
equals "authenticated without the reader role is forbidden (403)" "403" \
  node curl -sk -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN" "https://$POD_IP:8443/metrics"
k create sa scraper2 -n $NS >/dev/null
TOKEN2=$(k -n $NS create token scraper2)
k create clusterrolebinding e2e-scraper2 --clusterrole=backshots-metrics-reader --serviceaccount=$NS:scraper2 >/dev/null
equals "with the metrics-reader role it works (200)" "200" \
  node curl -sk -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOKEN2" "https://$POD_IP:8443/metrics"
check "the health gauge is exported" bash -c "docker exec $CLUSTER-control-plane curl -sk -H 'Authorization: Bearer $TOKEN2' https://$POD_IP:8443/metrics | grep -q 'backshots_policy_healthy{namespace=\"$NS\",policy=\"pg\"} 1'"

step "Result"
echo "  $pass passed, $fail failed"
((fail == 0))
