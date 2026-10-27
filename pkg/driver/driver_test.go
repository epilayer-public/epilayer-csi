package driver

import (
	"context"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestTopologyRegionKey(t *testing.T) {
	for _, tc := range []struct {
		driverName string
		want       string
	}{
		{DefaultDriverName, "topology.csi.epilayer.eu/region"},
		{"csi.sagadata.no", "topology.csi.sagadata.no/region"},
	} {
		d := &Driver{config: &Config{DriverName: tc.driverName}}
		if got := d.topologyRegionKey(); got != tc.want {
			t.Errorf("topologyRegionKey() with driver name %q = %q, want %q", tc.driverName, got, tc.want)
		}
	}
}

func TestGetPluginCapabilitiesAdvertisesOnlineExpansion(t *testing.T) {
	d := &Driver{config: &Config{}}
	resp, err := d.GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("GetPluginCapabilities() error = %v", err)
	}
	for _, c := range resp.Capabilities {
		if c.GetVolumeExpansion().GetType() == csi.PluginCapability_VolumeExpansion_ONLINE {
			return
		}
	}
	t.Errorf("GetPluginCapabilities() = %v, want VolumeExpansion ONLINE", resp.Capabilities)
}

func TestRequiredDeviceBytes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		capacityRange *csi.CapacityRange
		volumeSizeGiB int
		want          int64
	}{
		{"requested capacity", &csi.CapacityRange{RequiredBytes: 1536 * 1024 * 1024}, 2, 1536 * 1024 * 1024},
		{"no capacity range", nil, 2, 2 * giB},
		{"zero required bytes", &csi.CapacityRange{LimitBytes: 4 * giB}, 3, 3 * giB},
	} {
		if got := requiredDeviceBytes(tc.capacityRange, tc.volumeSizeGiB); got != tc.want {
			t.Errorf("%s: requiredDeviceBytes() = %d, want %d", tc.name, got, tc.want)
		}
	}
}
