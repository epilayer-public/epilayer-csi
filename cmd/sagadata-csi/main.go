package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sagadata-public/sagadata-csi/pkg/driver"
	"k8s.io/klog/v2"
)

var version = "dev"

func main() {
	klog.InitFlags(nil)

	var (
		endpoint = flag.String("endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint")
		mode     = flag.String("mode", "all", "Driver mode: controller, node, or all")
		nodeName = flag.String("node-name", "", "Kubernetes node name (node mode only, overrides NODE_NAME env)")
	)
	flag.Parse()

	apiEndpoint := os.Getenv("ENDPOINT")
	if apiEndpoint == "" {
		klog.Fatal("ENDPOINT environment variable is required")
	}

	tokenFile := os.Getenv("TOKEN_FILE")
	if tokenFile == "" {
		klog.Fatal("TOKEN_FILE environment variable is required")
	}

	region := os.Getenv("REGION")
	if region == "" {
		klog.Fatal("REGION environment variable is required")
	}

	nn := *nodeName
	if nn == "" {
		nn = os.Getenv("NODE_NAME")
	}

	m := driver.Mode(*mode)
	switch m {
	case driver.ModeController, driver.ModeNode, driver.ModeAll:
	default:
		klog.Fatalf("invalid mode %q, must be controller, node, or all", *mode)
	}

	cfg := &driver.Config{
		Endpoint:    *endpoint,
		Mode:        m,
		Version:     version,
		APIEndpoint: apiEndpoint,
		TokenFile:   tokenFile,
		Region:      region,
		NodeName:    nn,
	}

	drv, err := driver.NewDriver(cfg)
	if err != nil {
		klog.Fatalf("creating driver: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := drv.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
