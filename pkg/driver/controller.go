package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	sagadata "github.com/sagadata-public/sagadata-go"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

const (
	paramType          = "type"
	defaultVolumeType  = "ssd"
	minVolumeSizeBytes = 1 * giB
	maxVolumeSizeBytes = 10 * tiB

	giB = 1 << 30
	tiB = 1 << 40

	topologyRegionKey = "topology.csi.sagadata.no/region"

	// maxVirtioSerial is the maximum length of a virtio serial number,
	// which determines the volume name visible at /dev/disk/by-id/virtio-<name>.
	maxVirtioSerial = 20

	// volumeNamePrefix is prepended to the hash to identify CSI-managed volumes.
	volumeNamePrefix = "sd-"
)

func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}

	if len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}

	for _, cap := range req.VolumeCapabilities {
		if cap.GetBlock() != nil {
			return nil, status.Error(codes.InvalidArgument, "block volumes are not supported")
		}
		if cap.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			return nil, status.Errorf(codes.InvalidArgument, "unsupported access mode: %s", cap.GetAccessMode().GetMode())
		}
	}

	// Determine size.
	sizeBytes := int64(minVolumeSizeBytes)
	if req.CapacityRange != nil {
		if req.CapacityRange.RequiredBytes > 0 {
			sizeBytes = req.CapacityRange.RequiredBytes
		}
		if req.CapacityRange.LimitBytes > 0 && sizeBytes > req.CapacityRange.LimitBytes {
			return nil, status.Errorf(codes.OutOfRange, "requested size %d exceeds limit %d", sizeBytes, req.CapacityRange.LimitBytes)
		}
	}

	sizeGiB := roundUpGiB(sizeBytes)

	// Determine volume type.
	volType := defaultVolumeType
	if t, ok := req.Parameters[paramType]; ok {
		switch t {
		case "ssd", "hdd":
			volType = t
		default:
			return nil, status.Errorf(codes.InvalidArgument, "invalid volume type %q, must be ssd or hdd", t)
		}
	}

	// Generate a short volume name that fits in the virtio serial limit (20 chars).
	// The full PV name is stored in the volume description for traceability.
	volName := shortVolumeName(req.Name)

	// Idempotency: check if a volume with this name already exists.
	existing, err := d.getVolumeByName(ctx, volName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checking existing volume: %v", err)
	}
	if existing != nil {
		// Volume exists. Verify it matches the request.
		if existing.Size != sizeGiB {
			return nil, status.Errorf(codes.AlreadyExists, "volume %q exists with size %d GiB, requested %d GiB", volName, existing.Size, sizeGiB)
		}
		if string(existing.Type) != volType {
			return nil, status.Errorf(codes.AlreadyExists, "volume %q exists with type %s, requested %s", volName, existing.Type, volType)
		}
		klog.Infof("CreateVolume: volume %q already exists as %s", volName, existing.Id)
		return &csi.CreateVolumeResponse{
			Volume: volumeToCSI(existing, d.config.Region),
		}, nil
	}

	// Create the volume.
	vt := sagadata.VolumeType(volType)
	desc := fmt.Sprintf("csi:%s", req.Name)
	createResp, err := d.client.CreateVolumeWithResponse(ctx, sagadata.CreateVolumeJSONRequestBody{
		Name:        volName,
		Description: &desc,
		Region:      sagadata.Region(d.config.Region),
		Size:        sizeGiB,
		Type:        &vt,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "creating volume: %v", err)
	}
	if createResp.JSON201 == nil {
		return nil, status.Errorf(codes.Internal, "unexpected response creating volume: %s", createResp.Status())
	}

	vol := &createResp.JSON201.Volume
	klog.Infof("CreateVolume: created volume %q (id=%s, size=%dGiB, type=%s)", vol.Name, vol.Id, vol.Size, vol.Type)

	// Wait for volume to become ready.
	vol, err = d.waitForVolumeStatus(ctx, vol.Id, sagadata.VolumeStatusCreated, 2*time.Minute)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "waiting for volume to be ready: %v", err)
	}

	return &csi.CreateVolumeResponse{
		Volume: volumeToCSI(vol, d.config.Region),
	}, nil
}

func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	// Check if the volume is still attached; detach first if so.
	vol, err := d.getVolume(ctx, req.VolumeId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "getting volume %q: %v", req.VolumeId, err)
	}
	if vol == nil {
		klog.Infof("DeleteVolume: volume %q already gone", req.VolumeId)
		return &csi.DeleteVolumeResponse{}, nil
	}

	for _, inst := range vol.Instances {
		klog.Infof("DeleteVolume: detaching volume %q from instance %q before deletion", req.VolumeId, inst.Id)
		var volumes sagadata.InstanceUpdateVolumes
		if err := volumes.FromInstanceUpdateVolumesDetach(sagadata.InstanceUpdateVolumesDetach{
			Detach: req.VolumeId,
		}); err != nil {
			return nil, status.Errorf(codes.Internal, "building detach request: %v", err)
		}
		detachResp, err := d.client.UpdateInstanceWithResponse(ctx, inst.Id, sagadata.UpdateInstanceJSONRequestBody{
			Volumes: &volumes,
		})
		if err != nil {
			return nil, status.Errorf(codes.Internal, "detaching volume %q from instance %q: %v", req.VolumeId, inst.Id, err)
		}
		if detachResp.JSON200 == nil {
			return nil, status.Errorf(codes.Internal, "unexpected response detaching volume %q from instance %q: %s", req.VolumeId, inst.Id, detachResp.Status())
		}
	}

	resp, err := d.client.DeleteVolumeWithResponse(ctx, req.VolumeId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "deleting volume %q: %v", req.VolumeId, err)
	}

	// 404 is fine — idempotent delete.
	if resp.StatusCode() != http.StatusNoContent && resp.StatusCode() != http.StatusNotFound {
		return nil, status.Errorf(codes.Internal, "unexpected response deleting volume %q: %s", req.VolumeId, resp.Status())
	}

	klog.Infof("DeleteVolume: deleted volume %q", req.VolumeId)
	return &csi.DeleteVolumeResponse{}, nil
}

func (d *Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.NodeId == "" {
		return nil, status.Error(codes.InvalidArgument, "node ID is required")
	}

	if req.VolumeCapability.GetBlock() != nil {
		return nil, status.Error(codes.InvalidArgument, "block volumes are not supported")
	}

	// Check if volume exists.
	vol, err := d.getVolume(ctx, req.VolumeId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "getting volume: %v", err)
	}
	if vol == nil {
		return nil, status.Errorf(codes.NotFound, "volume %q not found", req.VolumeId)
	}

	// Check if already attached to this node.
	for _, inst := range vol.Instances {
		if inst.Id == req.NodeId {
			klog.Infof("ControllerPublishVolume: volume %q already attached to %q", req.VolumeId, req.NodeId)
			return &csi.ControllerPublishVolumeResponse{
				PublishContext: map[string]string{
					"volumeName": vol.Name,
				},
			}, nil
		}
	}

	// If attached to a different node, fail.
	if len(vol.Instances) > 0 {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %q already attached to instance %q", req.VolumeId, vol.Instances[0].Id)
	}

	// Attach.
	var volumes sagadata.InstanceUpdateVolumes
	if err := volumes.FromInstanceUpdateVolumesAttach(sagadata.InstanceUpdateVolumesAttach{
		Attach: req.VolumeId,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "building attach request: %v", err)
	}

	updateResp, err := d.client.UpdateInstanceWithResponse(ctx, req.NodeId, sagadata.UpdateInstanceJSONRequestBody{
		Volumes: &volumes,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "attaching volume %q to instance %q: %v", req.VolumeId, req.NodeId, err)
	}
	if updateResp.JSON200 == nil {
		return nil, status.Errorf(codes.Internal, "unexpected response attaching volume: %s", updateResp.Status())
	}

	klog.Infof("ControllerPublishVolume: attached volume %q to instance %q", req.VolumeId, req.NodeId)

	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{
			"volumeName": vol.Name,
		},
	}, nil
}

func (d *Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	// If no node ID, we can't detach.
	if req.NodeId == "" {
		return nil, status.Error(codes.InvalidArgument, "node ID is required")
	}

	// Check current state.
	vol, err := d.getVolume(ctx, req.VolumeId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "getting volume: %v", err)
	}
	if vol == nil {
		// Volume gone — nothing to detach.
		klog.Infof("ControllerUnpublishVolume: volume %q not found, nothing to detach", req.VolumeId)
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	// Check if attached to this node.
	attached := false
	for _, inst := range vol.Instances {
		if inst.Id == req.NodeId {
			attached = true
			break
		}
	}
	if !attached {
		klog.Infof("ControllerUnpublishVolume: volume %q not attached to %q", req.VolumeId, req.NodeId)
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}

	// Detach.
	var volumes sagadata.InstanceUpdateVolumes
	if err := volumes.FromInstanceUpdateVolumesDetach(sagadata.InstanceUpdateVolumesDetach{
		Detach: req.VolumeId,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "building detach request: %v", err)
	}

	updateResp, err := d.client.UpdateInstanceWithResponse(ctx, req.NodeId, sagadata.UpdateInstanceJSONRequestBody{
		Volumes: &volumes,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "detaching volume %q from instance %q: %v", req.VolumeId, req.NodeId, err)
	}
	if updateResp.JSON200 == nil {
		return nil, status.Errorf(codes.Internal, "unexpected response detaching volume: %s", updateResp.Status())
	}

	klog.Infof("ControllerUnpublishVolume: detached volume %q from instance %q", req.VolumeId, req.NodeId)
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if len(req.VolumeCapabilities) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}

	vol, err := d.getVolume(ctx, req.VolumeId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "getting volume: %v", err)
	}
	if vol == nil {
		return nil, status.Errorf(codes.NotFound, "volume %q not found", req.VolumeId)
	}

	for _, cap := range req.VolumeCapabilities {
		if cap.GetBlock() != nil {
			return &csi.ValidateVolumeCapabilitiesResponse{}, nil
		}
		if cap.GetAccessMode().GetMode() != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			return &csi.ValidateVolumeCapabilitiesResponse{}, nil
		}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.VolumeCapabilities,
		},
	}, nil
}

func (d *Driver) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	// CSI uses an opaque token for pagination; we use the page number as the token.
	page := 1
	if req.StartingToken != "" {
		if _, err := fmt.Sscanf(req.StartingToken, "%d", &page); err != nil {
			return nil, status.Errorf(codes.Aborted, "invalid starting token %q", req.StartingToken)
		}
	}

	perPage := int(req.MaxEntries)
	if perPage <= 0 {
		perPage = 100
	}

	resp, err := d.client.ListVolumesPaginatedWithResponse(ctx, &sagadata.ListVolumesPaginatedParams{
		Page:    &page,
		PerPage: &perPage,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing volumes: %v", err)
	}
	if resp.JSON200 == nil {
		return nil, status.Errorf(codes.Internal, "unexpected response: %s", resp.Status())
	}

	entries := make([]*csi.ListVolumesResponse_Entry, 0, len(resp.JSON200.Volumes))
	for idx := range resp.JSON200.Volumes {
		vol := &resp.JSON200.Volumes[idx]
		publishedNodeIDs := make([]string, 0, len(vol.Instances))
		for _, inst := range vol.Instances {
			publishedNodeIDs = append(publishedNodeIDs, inst.Id)
		}
		entries = append(entries, &csi.ListVolumesResponse_Entry{
			Volume: volumeToCSI(vol, d.config.Region),
			Status: &csi.ListVolumesResponse_VolumeStatus{
				PublishedNodeIds: publishedNodeIDs,
			},
		})
	}

	var nextToken string
	if page*resp.JSON200.PerPage < resp.JSON200.TotalCount {
		nextToken = fmt.Sprintf("%d", page+1)
	}

	return &csi.ListVolumesResponse{
		Entries:   entries,
		NextToken: nextToken,
	}, nil
}

func (d *Driver) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	caps := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES_PUBLISHED_NODES,
	}

	var csiCaps []*csi.ControllerServiceCapability
	for _, c := range caps {
		csiCaps = append(csiCaps, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{
					Type: c,
				},
			},
		})
	}

	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: csiCaps,
	}, nil
}

// Unimplemented controller RPCs.

func (d *Driver) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "CreateSnapshot not supported")
}

func (d *Driver) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "DeleteSnapshot not supported")
}

func (d *Driver) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ListSnapshots not supported")
}

func (d *Driver) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerExpandVolume not supported")
}

func (d *Driver) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest) (*csi.ControllerGetVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerGetVolume not supported")
}

func (d *Driver) ControllerModifyVolume(ctx context.Context, req *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerModifyVolume not supported")
}

func (d *Driver) GroupControllerGetCapabilities(ctx context.Context, req *csi.GroupControllerGetCapabilitiesRequest) (*csi.GroupControllerGetCapabilitiesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "GroupControllerGetCapabilities not supported")
}

func (d *Driver) CreateVolumeGroupSnapshot(ctx context.Context, req *csi.CreateVolumeGroupSnapshotRequest) (*csi.CreateVolumeGroupSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "CreateVolumeGroupSnapshot not supported")
}

func (d *Driver) DeleteVolumeGroupSnapshot(ctx context.Context, req *csi.DeleteVolumeGroupSnapshotRequest) (*csi.DeleteVolumeGroupSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "DeleteVolumeGroupSnapshot not supported")
}

func (d *Driver) GetVolumeGroupSnapshot(ctx context.Context, req *csi.GetVolumeGroupSnapshotRequest) (*csi.GetVolumeGroupSnapshotResponse, error) {
	return nil, status.Error(codes.Unimplemented, "GetVolumeGroupSnapshot not supported")
}

// Helpers.

func volumeToCSI(vol *sagadata.Volume, region string) *csi.Volume {
	return &csi.Volume{
		VolumeId:      vol.Id,
		CapacityBytes: int64(vol.Size) * giB,
		AccessibleTopology: []*csi.Topology{
			{
				Segments: map[string]string{
					topologyRegionKey: region,
				},
			},
		},
	}
}

func roundUpGiB(sizeBytes int64) int {
	return int((sizeBytes + giB - 1) / giB)
}

// shortVolumeName produces a deterministic volume name that fits within the
// 20-char virtio serial limit. Format: "sd-" + 17 hex chars of SHA-256.
// This gives 68 bits of entropy — collision probability is negligible.
func shortVolumeName(pvName string) string {
	h := sha256.Sum256([]byte(pvName))
	hashHex := hex.EncodeToString(h[:])
	maxHash := maxVirtioSerial - len(volumeNamePrefix)
	return volumeNamePrefix + hashHex[:maxHash]
}

func (d *Driver) waitForVolumeStatus(ctx context.Context, volumeID string, target sagadata.VolumeStatus, timeout time.Duration) (*sagadata.Volume, error) {
	deadline := time.Now().Add(timeout)
	for {
		vol, err := d.getVolume(ctx, volumeID)
		if err != nil {
			return nil, err
		}
		if vol == nil {
			return nil, fmt.Errorf("volume %q disappeared while waiting", volumeID)
		}
		if vol.Status == target {
			return vol, nil
		}
		if vol.Status == sagadata.VolumeStatusError {
			return nil, fmt.Errorf("volume %q entered error state", volumeID)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for volume %q to reach status %q (current: %q)", volumeID, target, vol.Status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}
