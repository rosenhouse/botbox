package cluster_test

import (
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	"github.com/rosenhouse/botbox/pkg/cluster"
)

func TestNewRESTMapperReportsADiscoveryThatFailed(t *testing.T) {
	unreachable := &rest.Config{Host: "http://127.0.0.1:1"}

	mapper, err := cluster.NewRESTMapper(unreachable)

	if err == nil {
		t.Fatalf("NewRESTMapper discovered %v against an API server that is not listening.", mapper)
	}
	if !strings.Contains(err.Error(), "discovering") {
		t.Errorf("NewRESTMapper returned %q, which does not say that discovery failed.", err)
	}
}

func TestNewRESTMapperReportsAClientItCannotBuild(t *testing.T) {
	absentCA := &rest.Config{Host: "https://127.0.0.1:1"}
	absentCA.TLSClientConfig.CAFile = "/no/such/ca.crt"

	mapper, err := cluster.NewRESTMapper(absentCA)

	if err == nil {
		t.Fatalf("NewRESTMapper built %v from a configuration naming a CA file that is not there.", mapper)
	}
	if !strings.Contains(err.Error(), "client") {
		t.Errorf("NewRESTMapper returned %q, which does not say that it could not build a client.", err)
	}
}
