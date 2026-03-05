# Saga Data CSI Driver — Implementation Plan

## Overview

A Kubernetes CSI (Container Storage Interface) driver for Saga Data Cloud that
provisions block volumes (HDD/SSD), attaches them to instances, and
formats/mounts them into pods. Ships as a single Go binary running in two
modes: **controller** (Deployment) and **node** (DaemonSet).

## Architecture

```
┌──────────────────────────────────────────────────────────────────────┐
│  Kubernetes Control Plane                                            │
│                                                                      │
│  ┌──────────────────────────────────────────────────┐                │
│  │  Controller Deployment (1 replica)                │                │
│  │  ┌──────────────┐ ┌──────────────┐ ┌───────────┐ │                │
│  │  │ csi-provisioner│ │ csi-attacher │ │ sagadata  │ │                │
│  │  │  (sidecar)    │ │  (sidecar)   │ │  -csi     │ │                │
│  │  │               │ │              │ │ --mode=   │ │                │
│  │  │               │ │              │ │ controller│ │                │
│  │  └──────┬────────┘ └──────┬───────┘ └─────┬─────┘ │                │
│  │         │  gRPC           │  gRPC         │       │                │
│  │         └─────────────────┴───────────────┘       │                │
│  └───────────────────────────────────────────────────┘                │
│                                                                      │
│  ┌──────────────────────────────────────────────────┐  (per node)    │
│  │  Node DaemonSet                                   │                │
│  │  ┌──────────────────┐  ┌─────────────────────┐    │                │
│  │  │ node-driver-     │  │ sagadata-csi         │    │                │
│  │  │ registrar        │  │ --mode=node          │    │                │
│  │  │  (sidecar)       │  │                      │    │                │
│  │  └──────┬───────────┘  └──────────┬───────────┘    │                │
│  │         │  gRPC                   │               │                │
│  │         └─────────────────────────┘               │                │
│  └───────────────────────────────────────────────────┘                │
│                                                                      │
│                        ▼  Saga Data API  ▼                           │
│             ┌─────────────────────────────────┐                      │
│             │  Volume Create / Delete          │                      │
│             │  Instance Attach / Detach        │                      │
│             └─────────────────────────────────┘                      │
└──────────────────────────────────────────────────────────────────────┘
```

## Device Path Convention

When a volume with **name** `X` is attached to an instance, it appears at:

    /dev/disk/by-id/virtio-X

The driver names volumes `pvc-<uuid>` (matching the PV name Kubernetes
generates), so the device path is `/dev/disk/by-id/virtio-pvc-<uuid>`.

## CSI RPCs Implemented

### Identity Service (both modes)

| RPC                    | Notes                                |
|------------------------|--------------------------------------|
| GetPluginInfo          | Returns driver name + version        |
| GetPluginCapabilities  | CONTROLLER_SERVICE, VOLUME_CONDITION |
| Probe                  | Health check (API reachable)         |

### Controller Service (controller mode)

| RPC                         | Notes                                        |
|-----------------------------|----------------------------------------------|
| CreateVolume                | Creates volume via API; params: type=ssd/hdd |
| DeleteVolume                | Deletes volume via API                       |
| ControllerPublishVolume     | Attaches volume to instance                  |
| ControllerUnpublishVolume   | Detaches volume from instance                |
| ValidateVolumeCapabilities  | Confirms RWO + FS mount support              |
| ListVolumes                 | Lists volumes, reports attached status        |
| ControllerGetCapabilities   | Advertises supported RPCs                    |

### Node Service (node mode)

| RPC                  | Notes                                            |
|----------------------|--------------------------------------------------|
| NodeStageVolume      | Detects FS, formats if needed, mounts to staging |
| NodeUnstageVolume    | Unmounts staging path                            |
| NodePublishVolume    | Bind-mounts staging → target path                |
| NodeUnpublishVolume  | Unmounts target path                             |
| NodeGetVolumeStats   | Returns capacity/usage via statfs                |
| NodeGetCapabilities  | STAGE_UNSTAGE_VOLUME, GET_VOLUME_STATS           |
| NodeGetInfo          | Returns instance ID + topology                   |

## StorageClass Parameters

| Parameter | Values      | Default | Description           |
|-----------|-------------|---------|-----------------------|
| `type`    | `ssd`, `hdd`| `ssd`   | Volume backing store  |

Example:
```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: sagadata-ssd
provisioner: csi.sagadata.no
parameters:
  type: ssd
allowVolumeExpansion: false   # future
volumeBindingMode: WaitForFirstConsumer
```

## Node Identity

The node plugin must know its own Saga Data instance ID to report in
`NodeGetInfo` (so the controller can attach volumes to the right instance).

Strategy:
1. `NODE_NAME` env var injected via Kubernetes downward API
2. On startup, query Saga Data API: find instance where
   `hostname == NODE_NAME || name == NODE_NAME`
3. Cache the instance ID for the lifetime of the process

This mirrors the approach used by the CCM's `instanceByNodeName`.

## Directory Structure

```
sagadata-csi/
├── cmd/
│   └── sagadata-csi/
│       └── main.go                 # Entrypoint: parse flags, start gRPC
├── pkg/
│   └── driver/
│       ├── driver.go               # Driver struct, gRPC server setup, Run()
│       ├── identity.go             # Identity service RPCs
│       ├── controller.go           # Controller service RPCs
│       ├── node.go                 # Node service RPCs
│       └── mounter.go              # Format/mount/unmount helpers (os/exec)
├── deploy/
│   └── kubernetes/
│       ├── controller.yaml         # Controller Deployment + ServiceAccount + RBAC
│       ├── node.yaml               # Node DaemonSet + ServiceAccount + RBAC
│       ├── csi-driver.yaml         # CSIDriver object registration
│       └── example-storageclass.yaml
├── Dockerfile
├── go.mod
├── go.sum
├── CLAUDE.md
├── README.md
└── plan.md
```

## Implementation Phases

### Phase 1: Project Skeleton

Files: `go.mod`, `cmd/sagadata-csi/main.go`, `pkg/driver/driver.go`

- Initialize Go module `github.com/sagadata-public/sagadata-csi`
- Depend on `github.com/sagadata-public/sagadata-go`,
  `github.com/container-storage-interface/spec`,
  `google.golang.org/grpc`, `k8s.io/mount-utils`, `k8s.io/klog/v2`
- `main.go`: parse flags (`--endpoint`, `--mode`, `--node-name`), env vars
  (`ENDPOINT`, `TOKEN_FILE`, `REGION`), create Saga Data client, start driver
- `driver.go`: `Driver` struct holding config + client + gRPC server;
  `Run()` listens on the CSI socket and registers the appropriate services
  based on mode

### Phase 2: Identity Service

File: `pkg/driver/identity.go`

- `GetPluginInfo` → name=`csi.sagadata.no`, version from build-time ldflags
- `GetPluginCapabilities` → `CONTROLLER_SERVICE`
- `Probe` → always ready (optionally ping the API)

### Phase 3: Controller Service

File: `pkg/driver/controller.go`

- **CreateVolume**: extract `type` from params, `size` from capacity range,
  call `CreateVolumeWithResponse`, poll until status=`created`, return
  volume ID. Use PV name as volume name. Support idempotency by checking
  if a volume with the same name already exists.
- **DeleteVolume**: call `DeleteVolumeWithResponse`. Idempotent (ignore 404).
- **ControllerPublishVolume**: attach volume to instance via
  `UpdateInstanceWithResponse` with `InstanceUpdateVolumesAttach`.
  Poll until the volume appears in the instance's volumes list.
- **ControllerUnpublishVolume**: detach via `InstanceUpdateVolumesDetach`.
  Idempotent (ignore if already detached).
- **ValidateVolumeCapabilities**: confirm RWO + mount.
- **ListVolumes**: paginate through `ListVolumesPaginatedWithResponse`.
- **ControllerGetCapabilities**: advertise CREATE_DELETE_VOLUME,
  PUBLISH_UNPUBLISH_VOLUME, LIST_VOLUMES.

### Phase 4: Node Service

Files: `pkg/driver/node.go`, `pkg/driver/mounter.go`

- **mounter.go**: thin wrappers around `k8s.io/mount-utils` and
  `os/exec` for `blkid`, `mkfs.ext4`, `mkfs.xfs`, mount, unmount.
- **NodeGetInfo**: return cached instance ID as node ID,
  topology `topology.csi.sagadata.no/region=<region>`.
- **NodeStageVolume**: resolve device at `/dev/disk/by-id/virtio-<vol-name>`,
  wait for device to appear (attach may still be propagating), detect
  existing filesystem with `blkid`. If none, format with requested fsType
  (default ext4). Mount to staging path.
- **NodeUnstageVolume**: unmount staging path.
- **NodePublishVolume**: bind-mount from staging to target path.
- **NodeUnpublishVolume**: unmount target path.
- **NodeGetVolumeStats**: `statfs` on the mount point.

### Phase 5: Dockerfile & Manifests

Files: `Dockerfile`, `deploy/kubernetes/*.yaml`

- **Dockerfile**: multi-stage, match CCM pattern (golang:1.25 builder,
  distroless runtime). Install e2fsprogs + xfsprogs in final image (needed
  for mkfs). Use a non-distroless base for node image since it needs
  filesystem tools — use `gcr.io/distroless/base-debian12` or Alpine.
- **controller.yaml**: Deployment (1 replica) with csi-provisioner +
  csi-attacher sidecars, RBAC for PV/PVC/VolumeAttachment/StorageClass.
- **node.yaml**: DaemonSet with node-driver-registrar sidecar,
  hostPath mounts for `/dev`, `/sys`, kubelet dir. Privileged container.
- **csi-driver.yaml**: `CSIDriver` object with `attachRequired: true`,
  `podInfoOnMount: true`, `fsGroupPolicy: File`.
- **example-storageclass.yaml**: SSD + HDD examples.

### Phase 6: Validation

- Deploy to a Saga Data Kubernetes cluster
- Create a PVC, verify volume appears in API
- Create a pod using the PVC, verify mount
- Delete the pod, verify unmount
- Delete the PVC, verify volume deletion

## Not in Scope (Future)

- Volume expansion (online resize)
- Volume snapshots (CreateSnapshot / RestoreSnapshot)
- Raw block volume mode
- Volume cloning
- Topology-aware scheduling beyond region
- Metrics / tracing
