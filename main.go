package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/cert-manager/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	"github.com/cert-manager/cert-manager/pkg/acme/webhook/cmd"
	"github.com/selectel/cert-manager-webhook-selectel/selectel"
	coreV1 "k8s.io/api/core/v1"
	extAPI "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

const (
	providerName    = "selectel"
	groupNameEnvVar = "GROUP_NAME"
)

func main() {
	groupName := os.Getenv("GROUP_NAME")
	if groupName == "" {
		panic(fmt.Sprintf("%s must be specified", groupNameEnvVar))
	}

	// We must setup logger
	// https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/log#pkg-variables
	// example from https://sdk.operatorframework.io/docs/building-operators/golang/references/logging/
	logger := zap.New()
	logf.SetLogger(logger)

	// This will register our custom DNS provider with the webhook serving
	// library, making it available as an API under the provided groupName.
	// You can register multiple DNS provider implementations with a single
	// webhook, where the Name() method will be used to disambiguate between
	// the different implementations.
	cmd.RunWebhookServer(groupName,
		&selectelDNSProviderSolver{},
	)
}

// selectelDNSProviderSolver implements the provider-specific logic needed to
// 'present' an ACME challenge TXT record for your own DNS provider.
// To do so, it must implement the
// `https://pkg.go.dev/github.com/cert-manager/cert-manager@v1.14.1/pkg/acme/webhook#Solver` interface.
type selectelDNSProviderSolver struct {
	client *kubernetes.Clientset
}

// selectelDNSProviderConfig is a structure that is used to decode into when
// solving a DNS01 challenge.
type selectelDNSProviderConfig struct {
	DNSSecretRef coreV1.SecretReference `json:"dnsSecretRef"`
	*selectel.Config
}

var ErrSecretForAuthNotSetup = errors.New("secret name not setup")

func (c *selectelDNSProviderSolver) validate(cfg *selectelDNSProviderConfig) error {
	if cfg.DNSSecretRef.Name == "" {
		return ErrSecretForAuthNotSetup
	}

	return nil
}

func (c *selectelDNSProviderSolver) provider(cfg *selectelDNSProviderConfig, namespace string) (*selectel.DNSProvider, error) {
	if err := c.validate(cfg); err != nil {
		return nil, err
	}
	sec, err := c.client.CoreV1().
		Secrets(namespace).
		Get(context.Background(), cfg.DNSSecretRef.Name, metaV1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting secret from k8s: %w", err)
	}
	err = cfg.CredentialsForDNS.FromMapBytes(sec.Data)
	if err != nil {
		return nil, fmt.Errorf("setup credentials from secret: %w", err)
	}
	dnsProvider, err := selectel.NewDNSProviderFromConfig(cfg.Config)
	if err != nil {
		return nil, fmt.Errorf("setup dns provider: %w", err)
	}

	return dnsProvider, nil
}

// Return DNS provider name.
func (c *selectelDNSProviderSolver) Name() string {
	return providerName
}

// Present is responsible for actually presenting the DNS record with the
// DNS provider.
// This method should tolerate being called multiple times with the same value.
// cert-manager itself will later perform a self check to ensure that the
// solver has correctly configured the DNS provider.
func (c *selectelDNSProviderSolver) Present(challengeRequest *v1alpha1.ChallengeRequest) error {
	cfg, err := loadConfig(challengeRequest.Config)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	provider, err := c.provider(&cfg, challengeRequest.ResourceNamespace)
	if err != nil {
		return fmt.Errorf("setup selectell dns provider: %w", err)
	}
	err = provider.Present(challengeRequest.ResolvedZone, challengeRequest.ResolvedFQDN, challengeRequest.Key)
	if err != nil {
		return fmt.Errorf("present: %w", err)
	}

	return nil
}

// CleanUp should delete the relevant TXT record from the DNS provider console.
// If multiple TXT records exist with the same record name (e.g.
// _acme-challenge.example.com) then **only** the record with the same `key`
// value provided on the ChallengeRequest should be cleaned up.
// This is in order to facilitate multiple DNS validations for the same domain
// concurrently.
func (c *selectelDNSProviderSolver) CleanUp(challengeRequest *v1alpha1.ChallengeRequest) error {
	cfg, err := loadConfig(challengeRequest.Config)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	provider, err := c.provider(&cfg, challengeRequest.ResourceNamespace)
	if err != nil {
		return fmt.Errorf("setup selectell dns provider: %w", err)
	}
	err = provider.CleanUp(challengeRequest.ResolvedZone, challengeRequest.ResolvedFQDN, challengeRequest.Key)
	if err != nil {
		return fmt.Errorf("cleanup: %w", err)
	}

	return nil
}

// Initialize will be called when the webhook first starts.
// This method can be used to instantiate the webhook, i.e. initialising
// connections or warming up caches.
// Typically, the kubeClientConfig parameter is used to build a Kubernetes
// client that can be used to fetch resources from the Kubernetes API, e.g.
// Secret resources containing credentials used to authenticate with DNS
// provider accounts.
// The stopCh can be used to handle early termination of the webhook, in cases
// where a SIGTERM or similar signal is sent to the webhook process.
func (c *selectelDNSProviderSolver) Initialize(kubeClientCfg *rest.Config, _ <-chan struct{}) error {
	cl, err := kubernetes.NewForConfig(kubeClientCfg)
	if err != nil {
		return fmt.Errorf("k8s clientset: %w", err)
	}
	c.client = cl

	return nil
}

// loadConfig is a small helper function that decodes JSON configuration into
// the typed config struct.
func loadConfig(cfgJSON *extAPI.JSON) (selectelDNSProviderConfig, error) {
	cfg := selectelDNSProviderConfig{}
	cfgDNS, err := selectel.NewConfigForDNS()
	if err != nil {
		return cfg, fmt.Errorf("setup selectel config: %w", err)
	}
	cfg.Config = cfgDNS
	// handle the 'base case' where no configuration has been provided
	if cfgJSON == nil {
		return cfg, nil
	}
	if err := json.Unmarshal(cfgJSON.Raw, &cfg); err != nil {
		return cfg, fmt.Errorf("error decoding solver config: %w", err)
	}

	return cfg, nil
}
