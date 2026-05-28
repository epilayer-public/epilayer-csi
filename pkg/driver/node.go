package driver

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

const (
	defaultFsType      = "ext4"
	deviceWaitTimeout  = 30 * time.Second
	deviceWaitInterval = 1 * time.Second
)

func (d *Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}
	if req.VolumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}

	if req.VolumeCapability.GetBlock() != nil {
		return nil, status.Error(codes.InvalidArgument, "block volumes are not supported")
	}

	mnt := req.VolumeCapability.GetMount()
	if mnt == nil {
		return nil, status.Error(codes.InvalidArgument, "mount volume capability is required")
	}

	fsType := mnt.FsType
	if fsType == "" {
		fsType = defaultFsType
	}

	mountOptions := mnt.MountFlags

	// The volume name is passed in publish context by the controller.
	volumeName := req.PublishContext["volumeName"]
	if volumeName == "" {
		// Fall back: fetch from API.
		vol, err := d.getVolume(ctx, req.VolumeId)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "getting volume: %v", err)
		}
		if vol == nil {
			return nil, status.Errorf(codes.NotFound, "volume %q not found", req.VolumeId)
		}
		volumeName = vol.Name
	}

	devicePath := GetDevicePath(volumeName)

	// Wait for the device to appear (attachment may still be propagating).
	if err := waitForDevice(ctx, devicePath); err != nil {
		return nil, status.Errorf(codes.Internal, "waiting for device %s: %v", devicePath, err)
	}

	// Check if already mounted.
	mounter := NewMounter()
	mounted, err := mounter.IsMountPoint(req.StagingTargetPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checking mount point %s: %v", req.StagingTargetPath, err)
	}
	if mounted {
		klog.Infof("NodeStageVolume: %s already mounted at %s", devicePath, req.StagingTargetPath)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	klog.Infof("NodeStageVolume: staging %s (device=%s, fsType=%s) at %s", req.VolumeId, devicePath, fsType, req.StagingTargetPath)
	if err := mounter.FormatAndMount(devicePath, req.StagingTargetPath, fsType, mountOptions); err != nil {
		return nil, status.Errorf(codes.Internal, "formatting/mounting: %v", err)
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (d *Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}

	mounter := NewMounter()
	mounted, err := mounter.IsMountPoint(req.StagingTargetPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checking mount point: %v", err)
	}
	if !mounted {
		klog.Infof("NodeUnstageVolume: %s not mounted", req.StagingTargetPath)
		return &csi.NodeUnstageVolumeResponse{}, nil
	}

	klog.Infof("NodeUnstageVolume: unmounting %s", req.StagingTargetPath)
	if err := mounter.Unmount(req.StagingTargetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "unmounting %s: %v", req.StagingTargetPath, err)
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path is required")
	}
	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	mounter := NewMounter()

	// Check if already mounted.
	mounted, err := mounter.IsMountPoint(req.TargetPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checking mount point: %v", err)
	}
	if mounted {
		klog.Infof("NodePublishVolume: %s already mounted at %s", req.VolumeId, req.TargetPath)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	if err := os.MkdirAll(req.TargetPath, 0750); err != nil {
		return nil, status.Errorf(codes.Internal, "creating target path: %v", err)
	}

	mountOptions := []string{"bind"}
	if req.Readonly {
		mountOptions = append(mountOptions, "ro")
	}

	klog.Infof("NodePublishVolume: bind-mounting %s to %s", req.StagingTargetPath, req.TargetPath)
	if err := mounter.Mount(req.StagingTargetPath, req.TargetPath, "", mountOptions); err != nil {
		return nil, status.Errorf(codes.Internal, "bind-mounting: %v", err)
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	mounter := NewMounter()
	mounted, err := mounter.IsMountPoint(req.TargetPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checking mount point: %v", err)
	}
	if !mounted {
		klog.Infof("NodeUnpublishVolume: %s not mounted", req.TargetPath)
		return &csi.NodeUnpublishVolumeResponse{}, nil
	}

	klog.Infof("NodeUnpublishVolume: unmounting %s", req.TargetPath)
	if err := mounter.Unmount(req.TargetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "unmounting %s: %v", req.TargetPath, err)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (d *Driver) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.VolumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume path is required")
	}

	// Verify the path exists.
	if _, err := os.Stat(req.VolumePath); os.IsNotExist(err) {
		return nil, status.Errorf(codes.NotFound, "volume path %s does not exist", req.VolumePath)
	}

	var statfs unix.Statfs_t
	if err := unix.Statfs(req.VolumePath, &statfs); err != nil {
		return nil, status.Errorf(codes.Internal, "statfs %s: %v", req.VolumePath, err)
	}

	totalBytes := int64(statfs.Blocks) * statfs.Bsize
	availBytes := int64(statfs.Bavail) * statfs.Bsize
	usedBytes := totalBytes - availBytes

	totalInodes := int64(statfs.Files)
	freeInodes := int64(statfs.Ffree)
	usedInodes := totalInodes - freeInodes

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Unit:      csi.VolumeUsage_BYTES,
				Total:     totalBytes,
				Available: availBytes,
				Used:      usedBytes,
			},
			{
				Unit:      csi.VolumeUsage_INODES,
				Total:     totalInodes,
				Available: freeInodes,
				Used:      usedInodes,
			},
		},
	}, nil
}

func (d *Driver) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.VolumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume path is required")
	}

	vol, err := d.getVolume(ctx, req.VolumeId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "getting volume: %v", err)
	}
	if vol == nil {
		return nil, status.Errorf(codes.NotFound, "volume %q not found", req.VolumeId)
	}

	devicePath := GetDevicePath(vol.Name)
	mounter := NewMounter()
	if _, err := mounter.ResizeFS(devicePath, req.VolumePath); err != nil {
		return nil, status.Errorf(codes.Internal, "resizing filesystem on %s: %v", devicePath, err)
	}

	klog.Infof("NodeExpandVolume: resized filesystem on %s (volume %q)", devicePath, req.VolumeId)

	return &csi.NodeExpandVolumeResponse{
		CapacityBytes: int64(vol.Size) * giB,
	}, nil
}

func (d *Driver) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
					},
				},
			},
		},
	}, nil
}

func (d *Driver) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{
		NodeId: d.nodeID,
		AccessibleTopology: &csi.Topology{
			Segments: map[string]string{
				topologyRegionKey: d.config.Region,
			},
		},
	}, nil
}

// waitForDevice waits for the device to appear at the given path.
func waitForDevice(ctx context.Context, devicePath string) error {
	deadline := time.Now().Add(deviceWaitTimeout)
	for {
		if IsDeviceReady(devicePath) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for device %s", devicePath)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(deviceWaitInterval):
		}
	}
}
