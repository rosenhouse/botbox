//go:build envtest

package cluster_test

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/kubernetes"

	"github.com/rosenhouse/botbox/pkg/cluster"
)

func TestStartServesAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	c, err := cluster.Start(ctx, cluster.Options{})
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Stop(); err != nil {
			t.Errorf("Stop returned an error: %v", err)
		}
	})

	cfg := c.Config()
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		t.Fatalf("Building a discovery client failed: %v", err)
	}
	version, err := discoveryClient.ServerVersion()
	if err != nil {
		t.Fatalf("Fetching the server version failed: %v", err)
	}
	if version.GitVersion == "" {
		t.Error("The server reported an empty GitVersion.")
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("Building a clientset failed: %v", err)
	}
	if _, err := clientset.CoreV1().Namespaces().Get(ctx, "default", metav1.GetOptions{}); err != nil {
		t.Fatalf("Getting the default namespace failed: %v", err)
	}
}
