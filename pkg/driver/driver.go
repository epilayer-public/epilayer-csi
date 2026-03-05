package driver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"

	"github.com/container-storage-interface/spec/lib/go/csi"
	sagadata "github.com/sagadata-public/sagadata-go"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
)

const (
	DriverName = "csi.sagadata.no"
)

// Mode is the operating mode of the driver.
type Mode string

const (
	ModeController Mode = "controller"
	ModeNode       Mode = "node"
	ModeAll        Mode = "all"
)

// Config holds the driver configuration.
type Config struct {
	Endpoint string // CSI gRPC socket path (e.g. unix:///csi/csi.sock)
	Mode     Mode
	Version  string

	// Saga Data API
	APIEndpoint string
	TokenFile   string
	Region      string

	// Node mode only
	NodeName string
}

// Driver implements the CSI spec for Saga Data volumes.
type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer

	config *Config
	client *sagadata.ClientWithResponses
	srv    *grpc.Server

	// Node identity, resolved once on startup in node mode.
	nodeID string
}

// NewDriver creates a new CSI driver.
func NewDriver(cfg *Config) (*Driver, error) {
	client, err := sagadata.NewSagaDataClient(sagadata.ClientConfig{
		Endpoint:  cfg.APIEndpoint,
		TokenFile: cfg.TokenFile,
	})
	if err != nil {
		return nil, fmt.Errorf("creating sagadata client: %w", err)
	}

	return &Driver{
		config: cfg,
		client: client,
	}, nil
}

// Run starts the gRPC server on the configured endpoint.
func (d *Driver) Run(ctx context.Context) error {
	u, err := url.Parse(d.config.Endpoint)
	if err != nil {
		return fmt.Errorf("parsing endpoint %q: %w", d.config.Endpoint, err)
	}

	if u.Scheme != "unix" {
		return fmt.Errorf("only unix:// endpoints are supported, got %q", d.config.Endpoint)
	}

	addr := u.Path
	if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing existing socket %q: %w", addr, err)
	}

	listener, err := net.Listen("unix", addr)
	if err != nil {
		return fmt.Errorf("listening on %q: %w", addr, err)
	}

	d.srv = grpc.NewServer(grpc.UnaryInterceptor(logInterceptor))

	// Identity service is always registered.
	csi.RegisterIdentityServer(d.srv, d)

	switch d.config.Mode {
	case ModeController:
		csi.RegisterControllerServer(d.srv, d)
	case ModeNode:
		csi.RegisterNodeServer(d.srv, d)
	case ModeAll:
		csi.RegisterControllerServer(d.srv, d)
		csi.RegisterNodeServer(d.srv, d)
	default:
		return fmt.Errorf("unknown mode: %q", d.config.Mode)
	}

	// Resolve node identity on startup if running in node mode.
	if d.config.Mode == ModeNode || d.config.Mode == ModeAll {
		if err := d.resolveNodeID(ctx); err != nil {
			return fmt.Errorf("resolving node ID: %w", err)
		}
	}

	klog.Infof("starting %s %s in %s mode on %s", DriverName, d.config.Version, d.config.Mode, d.config.Endpoint)

	// Shutdown on context cancellation.
	go func() {
		<-ctx.Done()
		klog.Info("shutting down gRPC server")
		d.srv.GracefulStop()
	}()

	return d.srv.Serve(listener)
}

// resolveNodeID queries the Saga Data API to find the instance ID for this node.
func (d *Driver) resolveNodeID(ctx context.Context) error {
	if d.config.NodeName == "" {
		return fmt.Errorf("NODE_NAME is required in node mode")
	}

	inst, err := d.instanceByNodeName(ctx, d.config.NodeName)
	if err != nil {
		return fmt.Errorf("looking up instance for node %q: %w", d.config.NodeName, err)
	}

	d.nodeID = inst.Id
	klog.Infof("resolved node %q to instance %q", d.config.NodeName, d.nodeID)
	return nil
}

// instanceByNodeName finds an instance by matching hostname or name.
// Mirrors the CCM's approach.
func (d *Driver) instanceByNodeName(ctx context.Context, nodeName string) (*sagadata.Instance, error) {
	page := 1
	for {
		resp, err := d.client.ListInstancesPaginatedWithResponse(ctx, &sagadata.ListInstancesPaginatedParams{
			Page: &page,
		})
		if err != nil {
			return nil, fmt.Errorf("listing instances: %w", err)
		}
		if resp.JSON200 == nil {
			return nil, fmt.Errorf("unexpected response: %s", resp.Status())
		}
		for idx := range resp.JSON200.Instances {
			inst := &resp.JSON200.Instances[idx]
			if inst.Hostname == nodeName || inst.Name == nodeName {
				return inst, nil
			}
		}
		if page*resp.JSON200.PerPage >= resp.JSON200.TotalCount {
			break
		}
		page++
	}
	return nil, fmt.Errorf("instance not found for node %q", nodeName)
}

// getVolumeByName finds a volume by name (used for idempotent CreateVolume).
func (d *Driver) getVolumeByName(ctx context.Context, name string) (*sagadata.Volume, error) {
	page := 1
	for {
		resp, err := d.client.ListVolumesPaginatedWithResponse(ctx, &sagadata.ListVolumesPaginatedParams{
			Page: &page,
		})
		if err != nil {
			return nil, fmt.Errorf("listing volumes: %w", err)
		}
		if resp.JSON200 == nil {
			return nil, fmt.Errorf("unexpected response: %s", resp.Status())
		}
		for idx := range resp.JSON200.Volumes {
			vol := &resp.JSON200.Volumes[idx]
			if vol.Name == name {
				return vol, nil
			}
		}
		if page*resp.JSON200.PerPage >= resp.JSON200.TotalCount {
			break
		}
		page++
	}
	return nil, nil // not found
}

// getVolume fetches a volume by ID. Returns nil if not found.
func (d *Driver) getVolume(ctx context.Context, volumeID string) (*sagadata.Volume, error) {
	resp, err := d.client.GetVolumeWithResponse(ctx, volumeID)
	if err != nil {
		return nil, fmt.Errorf("getting volume %q: %w", volumeID, err)
	}
	if resp.StatusCode() == http.StatusNotFound {
		return nil, nil
	}
	if resp.JSON200 == nil {
		return nil, fmt.Errorf("unexpected response getting volume %q: %s", volumeID, resp.Status())
	}
	return &resp.JSON200.Volume, nil
}

func logInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	klog.V(4).Infof(">> %s", info.FullMethod)
	resp, err := handler(ctx, req)
	if err != nil {
		klog.Errorf("<< %s: %v", info.FullMethod, err)
	} else {
		klog.V(4).Infof("<< %s: ok", info.FullMethod)
	}
	return resp, err
}
