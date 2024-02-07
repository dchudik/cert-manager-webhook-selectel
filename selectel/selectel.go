// Package selectel implements a DNS provider for solving the DNS-01 challenge using Selectel Domains API.
// Selectel Domain API v2 reference: https://developers.selectel.ru/docs/cloud-services/dns_api/dns_api_actual/
package selectel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/selectel/cert-manager-webhook-selectel/selectel/internal"
	domainsV2 "github.com/selectel/domains-go/pkg/v2"
	"github.com/selectel/go-selvpcclient/v3/selvpcclient"
)

const (
	defaultBaseURL     = "https://api.selectel.ru/domains/v2"
	minTTL             = 60
	defaultHTTPTimeout = 40

	userAgent               = "cert-manager-webhook-selectel"
	headerForOSProjectToken = "X-Auth-Token"
)

var (
	errTTLMustBeGreaterOrEqualsMinTTL = fmt.Errorf("ttl must be greater or equals min ttl: %d", minTTL)
	errConfigAbsent                   = fmt.Errorf("the configuration of the DNS provider is absent")
	errKeyNotFoundInSecret            = fmt.Errorf("key not found in secret")
)

// Config is used to configure the creation of the DNSProvider.
type Config struct {
	BaseURL           string            `json:"baseUrl"`
	TTL               int               `json:"ttl"`
	HTTPTimeout       int               `json:"httpTimeout"`
	CredentialsForDNS CredentialsForDNS `json:"-"`
}

type CredentialsForDNS struct {
	Username  string `json:"-"`
	Password  string `json:"-"`
	AccountID string `json:"-"`
	ProjectID string `json:"-"`
}

func (credentials *CredentialsForDNS) FromMapBytes(dataFromSecret map[string][]byte) error {
	var err error
	credentials.Username, err = getValueFromMapIfIsNotEmpty(dataFromSecret, "username")
	if err != nil {
		return fmt.Errorf("username: %w", err)
	}
	credentials.Password, err = getValueFromMapIfIsNotEmpty(dataFromSecret, "password")
	if err != nil {
		return fmt.Errorf("password: %w", err)
	}
	credentials.AccountID, err = getValueFromMapIfIsNotEmpty(dataFromSecret, "account_id")
	if err != nil {
		return fmt.Errorf("account_id: %w", err)
	}
	credentials.ProjectID, err = getValueFromMapIfIsNotEmpty(dataFromSecret, "project_id")
	if err != nil {
		return fmt.Errorf("project_id: %w", err)
	}

	return nil
}

func getValueFromMapIfIsNotEmpty(dataFromSecret map[string][]byte, key string) (string, error) {
	value, ok := dataFromSecret[key]
	if !ok && len(value) == 0 {
		return "", errKeyNotFoundInSecret
	}

	return string(value), nil
}

// NewDefaultConfig returns a default configuration for the DNSProvider.
func NewConfigForDNS() (*Config, error) {
	cfg := &Config{
		BaseURL:     defaultBaseURL,
		TTL:         minTTL,
		HTTPTimeout: defaultHTTPTimeout,
	}

	return cfg, nil
}

// DNSProvider is an implementation of the acme.ChallengeProvider interface.
type DNSProvider struct {
	config    *Config
	dnsClient domainsV2.DNSClient[domainsV2.Zone, domainsV2.RRSet]
}

// NewDNSProviderFromConfig return a DNSProvider instance configured for selectel.
func NewDNSProviderFromConfig(config *Config) (*DNSProvider, error) {
	if config == nil {
		return nil, errConfigAbsent
	}
	if config.TTL < minTTL {
		return nil, errTTLMustBeGreaterOrEqualsMinTTL
	}

	dnsClient, err := getDNSClientFromConfig(config)
	if err != nil {
		return nil, err
	}

	return &DNSProvider{
		config:    config,
		dnsClient: dnsClient,
	}, nil
}

// Present creates a recor in TXT RRSet to fulfill DNS-01 challenge.
func (d *DNSProvider) Present(zoneName, fqdn, value string) error {
	ctx := context.Background()
	zone, err := internal.GetZoneByName(ctx, d.dnsClient, zoneName)
	if err != nil {
		return fmt.Errorf("get zone by name: %w", err)
	}
	rrset, err := internal.GetRrsetByNameAndType(ctx, d.dnsClient, zone.ID, fqdn, string(domainsV2.TXT))
	if err != nil && !errors.Is(err, internal.ErrRrsetNotFound) {
		return fmt.Errorf("get rrset by name: %w", err)
	}
	// Escaping quotes in TXT record
	content := fmt.Sprintf("\"%s\"", value)
	// Create RRSet if not exists
	// else added one record to existing RRSet
	if errors.Is(err, internal.ErrRrsetNotFound) {
		createRrsetOpts := &domainsV2.RRSet{
			Name: fqdn,
			TTL:  d.config.TTL,
			Records: []domainsV2.RecordItem{
				{Content: content},
			},
			Type: domainsV2.TXT,
		}
		_, err = d.dnsClient.CreateRRSet(ctx, zone.ID, createRrsetOpts)
		if err != nil {
			return fmt.Errorf("create new rrset: %w", err)
		}
	} else {
		record := domainsV2.RecordItem{
			Content: content,
		}
		rrset.Records = append(rrset.Records, record)
		updateRrsetOpts := &domainsV2.RRSet{
			TTL:     rrset.TTL,
			Records: rrset.Records,
			Type:    domainsV2.TXT,
		}
		err = d.dnsClient.UpdateRRSet(ctx, zone.ID, rrset.ID, updateRrsetOpts)
		if err != nil {
			return fmt.Errorf("added record to existsing rrset: %w", err)
		}
	}

	return nil
}

// CleanUp removes a record from TXT RRSet used for DNS-01 challenge.
func (d *DNSProvider) CleanUp(zoneName, fqdn, value string) error {
	ctx := context.Background()
	zone, err := internal.GetZoneByName(ctx, d.dnsClient, zoneName)
	if err != nil {
		return fmt.Errorf("get zone by name: %w", err)
	}
	rrset, err := internal.GetRrsetByNameAndType(ctx, d.dnsClient, zone.ID, fqdn, string(domainsV2.TXT))
	if err != nil {
		return fmt.Errorf("get rrset by name and type: %w", err)
	}
	// if RRSet has one records delete rrset
	// else remove one record from RRSet
	if len(rrset.Records) == 1 {
		err = d.dnsClient.DeleteRRSet(ctx, zone.ID, rrset.ID)
		if err != nil {
			return fmt.Errorf("delete rrset: %w", err)
		}
	} else {
		newRecords := []domainsV2.RecordItem{}
		for i := range rrset.Records {
			// Escaping quotes in TXT record
			content := fmt.Sprintf("\"%s\"", value)
			if rrset.Records[i].Content != content {
				newRecords = append(newRecords, rrset.Records[i])
			}
		}
		err = d.dnsClient.UpdateRRSet(ctx, zone.ID, rrset.ID, &domainsV2.RRSet{
			TTL:     rrset.TTL,
			Records: newRecords,
		})
		if err != nil {
			return fmt.Errorf("delete one record from rrset: %w", err)
		}
	}

	return nil
}

func getDNSClientFromConfig(config *Config) (domainsV2.DNSClient[domainsV2.Zone, domainsV2.RRSet], error) {
	ctx := context.Background()
	options := &selvpcclient.ClientOptions{
		Context:    ctx,
		DomainName: config.CredentialsForDNS.AccountID,
		Username:   config.CredentialsForDNS.Username,
		Password:   config.CredentialsForDNS.Password,
		ProjectID:  config.CredentialsForDNS.ProjectID,
	}

	client, err := selvpcclient.NewClient(options)
	if err != nil {
		return nil, fmt.Errorf("setup selvpc client: %w", err)
	}

	projectToken := client.GetXAuthToken()
	hdrs := http.Header{}
	hdrs.Add(headerForOSProjectToken, projectToken)
	hdrs.Add("User-Agent", userAgent)

	httpClient := &http.Client{
		Timeout: time.Duration(config.HTTPTimeout) * time.Second,
	}
	domainsClient := domainsV2.NewClient(config.BaseURL, httpClient, hdrs)

	return domainsClient, nil
}
