package driver

import "testing"

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
