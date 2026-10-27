#!/usr/bin/env bash
# Online volume expansion test: creates a PVC and a pod writing to it, grows
# the PVC while the pod keeps running and checks that both the PVC capacity
# and the filesystem inside the pod reach the new size.
#
# Usage: test/e2e/expand.sh <kubeconfig> <storageclass> [initial-size] [new-size]
set -euo pipefail

if [ $# -lt 2 ]; then
  echo "usage: $0 <kubeconfig> <storageclass> [initial-size] [new-size]" >&2
  exit 2
fi

export KUBECONFIG=$1
STORAGE_CLASS=$2
INITIAL_SIZE=${3:-1Gi}
NEW_SIZE=${4:-2Gi}
TIMEOUT=${TIMEOUT:-300}
NAMESPACE=epilayer-csi-e2e-expand-$(date +%s)

to_bytes() {
  numfmt --from=auto "$1"
}

cleanup() {
  echo "cleaning up namespace $NAMESPACE"
  kubectl delete namespace "$NAMESPACE" --wait=true --timeout=300s || true
}
trap cleanup EXIT

kubectl create namespace "$NAMESPACE"

kubectl apply -n "$NAMESPACE" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $STORAGE_CLASS
  resources:
    requests:
      storage: $INITIAL_SIZE
---
apiVersion: v1
kind: Pod
metadata:
  name: writer
spec:
  containers:
    - name: writer
      image: busybox:1.37
      command:
        - sh
        - -c
        - while true; do date >> /data/log; sync; sleep 1; done
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: data
YAML

kubectl wait -n "$NAMESPACE" --for=condition=Ready pod/writer --timeout="${TIMEOUT}s"

echo "growing PVC from $INITIAL_SIZE to $NEW_SIZE"
kubectl patch -n "$NAMESPACE" pvc data --type=merge \
  -p "{\"spec\":{\"resources\":{\"requests\":{\"storage\":\"$NEW_SIZE\"}}}}"

new_bytes=$(to_bytes "$NEW_SIZE")
# The filesystem reserves space for its own metadata, so df reports a bit
# less than the device size.
min_fs_bytes=$((new_bytes * 9 / 10))

deadline=$((SECONDS + TIMEOUT))
pvc_ok=false
fs_ok=false
while [ $SECONDS -lt $deadline ]; do
  capacity=$(kubectl get -n "$NAMESPACE" pvc data -o jsonpath='{.status.capacity.storage}')
  if [ -n "$capacity" ] && [ "$(to_bytes "$capacity")" -ge "$new_bytes" ]; then
    pvc_ok=true
  fi
  fs_bytes=$(kubectl exec -n "$NAMESPACE" writer -- df -Pk /data | awk 'NR==2 {print $2 * 1024}')
  if [ "$fs_bytes" -ge "$min_fs_bytes" ]; then
    fs_ok=true
  fi
  echo "PVC capacity: ${capacity:-unknown}, filesystem size: $fs_bytes bytes"
  if $pvc_ok && $fs_ok; then
    break
  fi
  sleep 5
done

if ! $pvc_ok || ! $fs_ok; then
  echo "FAIL: expansion to $NEW_SIZE did not complete within ${TIMEOUT}s" >&2
  kubectl describe -n "$NAMESPACE" pvc data >&2 || true
  exit 1
fi

if ! kubectl get -n "$NAMESPACE" pod writer -o jsonpath='{.status.containerStatuses[0].restartCount}' | grep -qx 0; then
  echo "FAIL: writer pod restarted during expansion" >&2
  exit 1
fi

echo "PASS: PVC and filesystem grew to $NEW_SIZE online"
