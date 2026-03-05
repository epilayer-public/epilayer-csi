package driver

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
)

// Mounter wraps filesystem and mount operations.
type Mounter struct {
	mount.Interface
}

// NewMounter creates a new Mounter.
func NewMounter() *Mounter {
	return &Mounter{
		Interface: mount.New(""),
	}
}

// FormatAndMount formats the device with the given filesystem type and mounts
// it to the target path. If the device already has a filesystem, it is mounted
// directly without formatting.
func (m *Mounter) FormatAndMount(devicePath, targetPath, fsType string, mountOptions []string) error {
	existing, err := m.detectFilesystem(devicePath)
	if err != nil {
		return fmt.Errorf("detecting filesystem on %s: %w", devicePath, err)
	}

	if existing == "" {
		klog.Infof("formatting %s as %s", devicePath, fsType)
		if err := m.format(devicePath, fsType); err != nil {
			return fmt.Errorf("formatting %s: %w", devicePath, err)
		}
	} else {
		klog.Infof("device %s already has filesystem %s", devicePath, existing)
		if existing != fsType {
			return fmt.Errorf("device %s has filesystem %s, expected %s", devicePath, existing, fsType)
		}
	}

	if err := os.MkdirAll(targetPath, 0750); err != nil {
		return fmt.Errorf("creating mount point %s: %w", targetPath, err)
	}

	klog.Infof("mounting %s to %s (fsType=%s, opts=%v)", devicePath, targetPath, fsType, mountOptions)
	return m.Mount(devicePath, targetPath, fsType, mountOptions)
}

// detectFilesystem returns the filesystem type on the device, or empty string
// if the device has no filesystem.
func (m *Mounter) detectFilesystem(devicePath string) (string, error) {
	out, err := exec.Command("blkid", "-o", "value", "-s", "TYPE", devicePath).CombinedOutput()
	if err != nil {
		// blkid returns exit code 2 if the device has no filesystem.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 2 {
			return "", nil
		}
		return "", fmt.Errorf("blkid %s: %w (output: %s)", devicePath, err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

// format formats the device with the given filesystem type.
func (m *Mounter) format(devicePath, fsType string) error {
	var cmd *exec.Cmd
	switch fsType {
	case "ext4":
		cmd = exec.Command("mkfs.ext4", "-F", devicePath)
	case "xfs":
		cmd = exec.Command("mkfs.xfs", "-f", devicePath)
	default:
		return fmt.Errorf("unsupported filesystem type: %s", fsType)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (output: %s)", cmd.Path, err, string(out))
	}
	return nil
}

// IsMountPoint checks if the path is a mount point.
func (m *Mounter) IsMountPoint(path string) (bool, error) {
	notMnt, err := m.IsLikelyNotMountPoint(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return !notMnt, nil
}

// Unmount unmounts the path.
func (m *Mounter) Unmount(path string) error {
	return m.Interface.Unmount(path)
}

// GetDevicePath returns the device path for a volume name.
func GetDevicePath(volumeName string) string {
	return fmt.Sprintf("/dev/disk/by-id/virtio-%s", volumeName)
}

// IsDeviceReady checks if the device exists and is a block device.
func IsDeviceReady(devicePath string) bool {
	_, err := os.Stat(devicePath)
	return err == nil
}

// ResizeFS resizes the filesystem on the device to fill the available space.
func (m *Mounter) ResizeFS(devicePath, mountPath string) (bool, error) {
	resizer := mount.NewResizeFs(utilexec.New())
	return resizer.Resize(devicePath, mountPath)
}
