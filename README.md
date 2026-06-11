# EpiLayer CSI Driver

A [Container Storage Interface](https://github.com/container-storage-interface/spec) driver for [EpiLayer](https://epilayer.eu) cloud block storage, enabling dynamic provisioning of persistent volumes in Kubernetes.

## Features

- Dynamic provisioning of SSD and HDD volumes via StorageClass parameters
- Automatic volume attachment to nodes
- Filesystem formatting (ext4, xfs) and mounting
- Idempotent create/delete/attach/detach operations
- Topology-aware provisioning (region)

## Limitations

- **`ReadWriteOnce` only.** EpiLayer block volumes are single-attach; a volume can be mounted by exactly one node at a time. `ReadWriteMany` (RWX), `ReadOnlyMany` (ROX), and `ReadWriteOncePod` are not supported. A PVC created with any of these access modes will remain stuck in `Pending` with a `ProvisioningFailed` event:
  ```
  unsupported access mode: MULTI_NODE_MULTI_WRITER
  ```
  Multiple pods on the **same node** sharing a `ReadWriteOnce` volume is fine — they receive individual bind-mounts from the shared staging mount.

- **No snapshots.** `CreateSnapshot` / `DeleteSnapshot` are not implemented.

## Installation

### Prerequisites

- Kubernetes cluster running on EpiLayer Cloud
- EpiLayer API token
- The [EpiLayer Cloud Controller Manager](https://github.com/epilayer-public/epilayer-cloud-controller-manager) configured and running

### Deploy

1. Create a secret with your API credentials:

```bash
kubectl create secret generic epilayer-csi \
  --namespace kube-system \
  --from-literal=token=<your-api-token> \
  --from-literal=endpoint=<api-endpoint> \
  --from-literal=region=<REGION>  # uppercase, e.g. NORD-NO-KRS-1
```

> **Note:** The endpoint must include the `/compute/v1` path prefix (e.g. `https://…/compute/v1`). Omitting it causes all API calls to 404.

2. Apply the manifests:

```bash
kubectl apply -f deploy/kubernetes/csi-driver.yaml
kubectl apply -f deploy/kubernetes/controller.yaml
kubectl apply -f deploy/kubernetes/node.yaml
```

3. Create a StorageClass:

```bash
kubectl apply -f deploy/kubernetes/example-storageclass.yaml
```

## Usage

Create a PVC:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-data
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: epilayer-ssd
  resources:
    requests:
      storage: 10Gi
```

Use it in a pod:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-app
spec:
  containers:
    - name: app
      image: nginx
      volumeMounts:
        - mountPath: /data
          name: data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: my-data
```

## Configuration

### StorageClass Parameters

| Parameter | Values       | Default | Description          |
|-----------|-------------|---------|----------------------|
| `type`    | `ssd`, `hdd` | `ssd`   | Volume backing store |

### Environment Variables

| Variable     | Description                    | Required |
|-------------|--------------------------------|----------|
| `ENDPOINT`  | EpiLayer API endpoint URL     | Yes      |
| `TOKEN_FILE`| Path to file containing token  | Yes      |
| `REGION`    | EpiLayer region identifier (uppercase, e.g. `NORD-NO-KRS-1`) | Yes |
| `NODE_NAME` | Kubernetes node name (node mode)| Node only|

## Building

```bash
CGO_ENABLED=0 go build -o epilayer-csi ./cmd/epilayer-csi
```

Docker:

```bash
docker build -t epilayer-csi .
```

## License

Mozilla Public License 2.0
