package main

import (
	"os"
	"testing"

	acmetest "github.com/cert-manager/cert-manager/test/acme"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var zone = os.Getenv("TEST_ZONE_NAME")

func TestRunsSuite(t *testing.T) {
	t.Parallel()
	// We must setup logger
	// https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/log#pkg-variables
	// example from https://sdk.operatorframework.io/docs/building-operators/golang/references/logging/
	logger := zap.New()
	logf.SetLogger(logger)
	// The manifest path should contain a file named config.json that is a
	// snippet of valid configuration that should be included on the
	// ChallengeRequest passed as part of the test cases.
	fixture := acmetest.NewFixture(&selectelDNSProviderSolver{},
		acmetest.SetResolvedZone(zone),
		acmetest.SetAllowAmbientCredentials(false),
		acmetest.SetManifestPath("testdata/selectel"),
		acmetest.SetStrict(true),
	)
	fixture.RunConformance(t)
}
