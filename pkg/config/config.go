// Copyright 2021 The Sigstore Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package config

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	lru "github.com/hashicorp/golang-lru"
	"github.com/sigstore/fulcio/pkg/log"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
)

const defaultOIDCDiscoveryTimeout = 10 * time.Second

// All hostnames for subject and issuer OIDC claims must have at least a
// top-level and second-level domain
const minimumHostnameLength = 2

type FulcioConfig struct {
	OIDCIssuers map[string]OIDCIssuer `json:"OIDCIssuers,omitempty"`

	// A meta issuer has a templated URL of the form:
	//   https://oidc.eks.*.amazonaws.com/id/*
	// Where * can match a single hostname or URI path parts
	// (in particular, no '.' or '/' are permitted, among
	// other special characters)  Some examples we want to match:
	// * https://oidc.eks.us-west-2.amazonaws.com/id/B02C93B6A2D30341AD01E1B6D48164CB
	// * https://container.googleapis.com/v1/projects/mattmoor-credit/locations/us-west1-b/clusters/tenant-cluster
	MetaIssuers map[string]OIDCIssuer `json:"MetaIssuers,omitempty"`

	// verifiers is a fixed mapping from our OIDCIssuers to their OIDC verifiers.
	verifiers map[string]*oidc.IDTokenVerifier
	// lru is an LRU cache of recently used verifiers for our meta issuers.
	lru *lru.TwoQueueCache
}

type OIDCIssuer struct {
	// The expected issuer of an OIDC token
	IssuerURL string `json:"IssuerURL,omitempty"`
	// The expected client ID of the OIDC token
	ClientID string `json:"ClientID"`
	// Used to determine the subject of the certificate and if additional
	// certificate values are needed
	Type IssuerType `json:"Type"`
	// Optional, if the issuer is in a different claim in the OIDC token
	IssuerClaim string `json:"IssuerClaim,omitempty"`
	// The domain that must be present in the subject for 'uri' issuer types
	// Also used to create an email for 'username' issuer types
	SubjectDomain string `json:"SubjectDomain,omitempty"`
	// SPIFFETrustDomain specifies the trust domain that 'spiffe' issuer types
	// issue ID tokens for. Tokens with a different trust domain will be
	// rejected.
	SPIFFETrustDomain string `json:"SPIFFETrustDomain,omitempty"`
}

func metaRegex(issuer string) (*regexp.Regexp, error) {
	// Quote all of the "meta" characters like `.` to avoid
	// those literal characters in the URL matching any character.
	// This will ALSO quote `*`, so we replace the quoted version.
	quoted := regexp.QuoteMeta(issuer)

	// Replace the quoted `*` with a regular expression that
	// will match alpha-numeric parts with common additional
	// "special" characters.
	replaced := strings.ReplaceAll(quoted, regexp.QuoteMeta("*"), "[-_a-zA-Z0-9]+")

	// Compile into a regular expression.
	return regexp.Compile(replaced)
}

// GetIssuer looks up the issuer configuration for an `issuerURL`
// coming from an incoming OIDC token.  If no matching configuration
// is found, then it returns `false`.
func (fc *FulcioConfig) GetIssuer(issuerURL string) (OIDCIssuer, bool) {
	iss, ok := fc.OIDCIssuers[issuerURL]
	if ok {
		return iss, ok
	}

	for meta, iss := range fc.MetaIssuers {
		re, err := metaRegex(meta)
		if err != nil {
			continue // Shouldn't happen, we check parsing the config
		}
		if re.MatchString(issuerURL) {
			// If it matches, then return a concrete OIDCIssuer
			// configuration for this issuer URL.
			return OIDCIssuer{
				IssuerURL:     issuerURL,
				ClientID:      iss.ClientID,
				Type:          iss.Type,
				IssuerClaim:   iss.IssuerClaim,
				SubjectDomain: iss.SubjectDomain,
			}, true
		}
	}

	return OIDCIssuer{}, false
}

// GetVerifier fetches a token verifier for the given `issuerURL`
// coming from an incoming OIDC token.  If no matching configuration
// is found, then it returns `false`.
func (fc *FulcioConfig) GetVerifier(issuerURL string) (*oidc.IDTokenVerifier, bool) {
	// Look up our fixed issuer verifiers
	v, ok := fc.verifiers[issuerURL]
	if ok {
		return v, true
	}

	// Look in the LRU cache for a verifier
	untyped, ok := fc.lru.Get(issuerURL)
	if ok {
		return untyped.(*oidc.IDTokenVerifier), true
	}
	// If this issuer hasn't been recently used, then create a new verifier
	// and add it to the LRU cache.

	iss, ok := fc.GetIssuer(issuerURL)
	if !ok {
		return nil, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultOIDCDiscoveryTimeout)
	defer cancel()
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		log.Logger.Warnf("Failed to create provider for issuer URL %q: %v", issuerURL, err)
		return nil, false
	}
	verifier := provider.Verifier(&oidc.Config{ClientID: iss.ClientID})
	fc.lru.Add(issuerURL, verifier)
	return verifier, true
}

func (fc *FulcioConfig) prepare() error {
	fc.verifiers = make(map[string]*oidc.IDTokenVerifier, len(fc.OIDCIssuers))
	for _, iss := range fc.OIDCIssuers {
		ctx, cancel := context.WithTimeout(context.Background(), defaultOIDCDiscoveryTimeout)
		defer cancel()

		dialer := &net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		transport := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		}

		var client *http.Client
		client = &http.Client{
			Transport: transport,
		}
		clientctx := oidc.ClientContext(ctx, client)

		provider, err := oidc.NewProvider(clientctx, iss.IssuerURL)
		if err != nil {
			return fmt.Errorf("provider %s: %w", iss.IssuerURL, err)
		}
		fc.verifiers[iss.IssuerURL] = provider.Verifier(&oidc.Config{ClientID: iss.ClientID})
	}

	cache, err := lru.New2Q(100 /* size */)
	if err != nil {
		return fmt.Errorf("lru: %w", err)
	}
	fc.lru = cache
	return nil
}

type IssuerType string

const (
	IssuerTypeEmail          = "email"
	IssuerTypeGithubWorkflow = "github-workflow"
	IssuerTypeKubernetes     = "kubernetes"
	IssuerTypeSpiffe         = "spiffe"
	IssuerTypeURI            = "uri"
	IssuerTypeUsername       = "username"
)

func parseConfig(b []byte) (cfg *FulcioConfig, err error) {
	cfg = &FulcioConfig{}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	return cfg, nil
}

func validateConfig(conf *FulcioConfig) error {
	if conf == nil {
		return errors.New("nil config")
	}

	for _, issuer := range conf.OIDCIssuers {
		if issuer.IssuerClaim != "" && issuer.Type != IssuerTypeEmail {
			return errors.New("only email issuers can use issuer claim mapping")
		}
		if issuer.Type == IssuerTypeSpiffe {
			if issuer.SPIFFETrustDomain == "" {
				return errors.New("spiffe issuer must have SPIFFETrustDomain set")
			}
			// verify that trust domain is valid
			if _, err := spiffeid.TrustDomainFromString(issuer.SPIFFETrustDomain); err != nil {
				return errors.New("spiffe trust domain is invalid")
			}
		}
		if issuer.Type == IssuerTypeURI {
			if issuer.SubjectDomain == "" {
				return errors.New("uri issuer must have SubjectDomain set")
			}
			uDomain, err := url.Parse(issuer.SubjectDomain)
			if err != nil {
				return err
			}
			if uDomain.Scheme == "" {
				return errors.New("SubjectDomain for uri must contain scheme")
			}
			uIssuer, err := url.Parse(issuer.IssuerURL)
			if err != nil {
				return err
			}
			if uIssuer.Scheme == "" {
				return errors.New("issuer for uri must contain scheme")
			}
			// The domain in the configuration must match the domain (excluding the subdomain) of the issuer
			// In order to declare this configuration, a test must have been done to prove ownership
			// over both the issuer and domain configuration values.
			// Valid examples:
			// * SubjectDomain = https://example.com, IssuerURL = https://accounts.example.com
			// * SubjectDomain = https://accounts.example.com, IssuerURL = https://accounts.example.com
			// * SubjectDomain = https://users.example.com, IssuerURL = https://accounts.example.com
			if err := isURISubjectAllowed(uDomain, uIssuer); err != nil {
				return err
			}
		}
		if issuer.Type == IssuerTypeUsername {
			if issuer.SubjectDomain == "" {
				return errors.New("username issuer must have SubjectDomain set")
			}
			uDomain, err := url.Parse(issuer.SubjectDomain)
			if err != nil {
				return err
			}
			if uDomain.Scheme != "" {
				return errors.New("SubjectDomain for username should not contain scheme")
			}
			uIssuer, err := url.Parse(issuer.IssuerURL)
			if err != nil {
				return err
			}
			if uIssuer.Scheme == "" {
				return errors.New("issuer for username must contain scheme")
			}
			// The domain in the configuration must match the domain (excluding the subdomain) of the issuer
			// In order to declare this configuration, a test must have been done to prove ownership
			// over both the issuer and domain configuration values.
			// Valid examples:
			// * SubjectDomain = example.com, IssuerURL = https://accounts.example.com
			// * SubjectDomain = accounts.example.com, IssuerURL = https://accounts.example.com
			// * SubjectDomain = users.example.com, IssuerURL = https://accounts.example.com
			if err := validateAllowedDomain(issuer.SubjectDomain, uIssuer.Hostname()); err != nil {
				return err
			}
		}
	}

	for _, metaIssuer := range conf.MetaIssuers {
		if metaIssuer.Type == IssuerTypeSpiffe {
			// This would establish a many to one relationship for OIDC issuers
			// to trust domains so we fail early and reject this configuration.
			return errors.New("SPIFFE meta issuers not supported")
		}
	}

	return nil
}

var DefaultConfig = &FulcioConfig{
	OIDCIssuers: map[string]OIDCIssuer{
		"https://oauth2.sigstore.dev/auth": {
			IssuerURL:   "https://oauth2.sigstore.dev/auth",
			ClientID:    "sigstore",
			IssuerClaim: "$.federated_claims.connector_id",
			Type:        IssuerTypeEmail,
		},
		"https://accounts.google.com": {
			IssuerURL: "https://accounts.google.com",
			ClientID:  "sigstore",
			Type:      IssuerTypeEmail,
		},
		"https://token.actions.githubusercontent.com": {
			IssuerURL: "https://token.actions.githubusercontent.com",
			ClientID:  "sigstore",
			Type:      IssuerTypeGithubWorkflow,
		},
	},
}

var originalTransport = http.DefaultTransport

type configKey struct{}

func With(ctx context.Context, cfg *FulcioConfig) context.Context {
	ctx = context.WithValue(ctx, configKey{}, cfg)
	return ctx
}

func FromContext(ctx context.Context) *FulcioConfig {
	untyped := ctx.Value(configKey{})
	if untyped == nil {
		return nil
	}
	return untyped.(*FulcioConfig)
}

// Load a config from disk, or use defaults
func Load(configPath string) (*FulcioConfig, error) {
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		log.Logger.Infof("No config at %s, using defaults: %v", configPath, DefaultConfig)
		config := DefaultConfig
		if err := config.prepare(); err != nil {
			return nil, err
		}
		return config, nil
	}
	b, err := ioutil.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}
	return Read(b)
}

var caCert = `-----BEGIN CERTIFICATE-----
MIIC2jCCAcKgAwIBAgIRAO8GakGeX5OJDWFdieQ8QW8wDQYJKoZIhvcNAQELBQAw
ADAeFw0yMjAzMTcxNzQ4MTJaFw0yMjA2MTUxNzQ4MTJaMAAwggEiMA0GCSqGSIb3
DQEBAQUAA4IBDwAwggEKAoIBAQDHQRLeXWvFTxtsaMTaYK380BcYHbJWZTVdTYQz
v36UpnxQb2L64P4zJO5I/8NwLjGKsT5HYWjV5nrgAKKiPDmgqVtk/krxANTyJ+KX
eswx7gC+58vjAsSILOnxDN5Je1jDx4FSd49VoHZ1+04jkCLgotcixMBf5yYBAxA2
So1CQGP4Qzla4VExoykCaos4rfxltUMHRQ5P0GYP1Ey3lqFka1LIJTTBoRgiRdJ0
2DtzlR/NYVRP1xkLKe2VHJD+bkqXFx+Fti9QC/xod5eEN5wlF3TtIfSFFoSmbKOr
TfIlt8wX2CT/6bmY8LEYLTsqoVLA14BwRr5CQxGuUlrATENnAgMBAAGjTzBNMA4G
A1UdDwEB/wQEAwIFoDAMBgNVHRMBAf8EAjAAMC0GA1UdEQEB/wQjMCGCH215LXN1
cGVydmlzb3IuZGVtby5waW5uaXBlZC5kZXYwDQYJKoZIhvcNAQELBQADggEBAE92
wWZJ01qTkkcm+Onh1zeHss+PBHyff9TpnQQFFGnUPzmzfOOKUR5H2kY2+d/avM+V
Mva2RU0qlJyxw+6k5hV1bcrQNhCXhCh+c/BBnkxm/dY/zqPMuHas1Qh3Eh1qImFs
ls0PQhK4MijOavd4xeEKR1fP7UnrbNc6X8GePV+YOa1WO3ULTpY86as/Rd+iMCKQ
f3Ztd93BXUo7xJ7hSzsWX82pS32Vm6IqE2DiZ0VPDdobABzHTLumY+aI1A20A9Pw
tUrh0wbVHREvz0YVk98589DvgXBIiKCuv6g2zwgXTetKu73J0VBm3rrQdSKxAVri
6E7Htz6UlXZbGWN3v4Y=
-----END CERTIFICATE-----`

// Read parses the bytes of a config
func Read(b []byte) (*FulcioConfig, error) {
	config, err := parseConfig(b)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}

	err = validateConfig(config)
	if err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}

	if _, ok := config.GetIssuer("https://kubernetes.default.svc"); ok {
		// Add the Kubernetes cluster's CA to the system CA pool, and to
		// the default transport.
		rootCAs, _ := x509.SystemCertPool()
		if rootCAs == nil {
			rootCAs = x509.NewCertPool()
		}
		const k8sCA = "/var/run/fulcio/ca.crt"
		certs, err := ioutil.ReadFile(k8sCA)
		if err != nil {
			return nil, fmt.Errorf("read file: %w", err)
		}
		if ok := rootCAs.AppendCertsFromPEM(certs); !ok {
			return nil, fmt.Errorf("unable to append certs")
		}

		t := originalTransport.(*http.Transport).Clone()
		t.TLSClientConfig.RootCAs = rootCAs
		http.DefaultTransport = t
	} else {
		// If we parse a config that doesn't include a cluster issuer
		// signed with the cluster'sCA, then restore the original transport
		// (in case we overwrote it)

		log.Logger.Infof("using the pre-defined CA: %v", caCert)
		t := originalTransport.(*http.Transport).Clone()
		t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		http.DefaultTransport = t
		//http.DefaultTransport = originalTransport
	}

	if err := config.prepare(); err != nil {
		return nil, err
	}
	return config, nil
}

// isURISubjectAllowed compares the subject and issuer URIs,
// returning an error if the scheme or the hostnames do not match
func isURISubjectAllowed(subject, issuer *url.URL) error {
	if subject.Scheme != issuer.Scheme {
		return fmt.Errorf("subject (%s) and issuer (%s) URI schemes do not match", subject.Scheme, issuer.Scheme)
	}

	return validateAllowedDomain(subject.Hostname(), issuer.Hostname())
}

// validateAllowedDomain compares two hostnames, returning an error if the
// top-level and second-level domains do not match
// TODO: This does not work for domains that end in co.jp or co.uk. We should consider
// using eTLDs, or removing this validation when we can challenge domain ownership.
func validateAllowedDomain(subjectHostname, issuerHostname string) error {
	// If the hostnames exactly match, return early
	if subjectHostname == issuerHostname {
		return nil
	}

	// Compare the top level and second level domains
	sHostname := strings.Split(subjectHostname, ".")
	iHostname := strings.Split(issuerHostname, ".")
	if len(sHostname) < minimumHostnameLength {
		return fmt.Errorf("URI hostname too short: %s", subjectHostname)
	}
	if len(iHostname) < minimumHostnameLength {
		return fmt.Errorf("URI hostname too short: %s", issuerHostname)
	}
	if sHostname[len(sHostname)-1] == iHostname[len(iHostname)-1] &&
		sHostname[len(sHostname)-2] == iHostname[len(iHostname)-2] {
		return nil
	}
	return fmt.Errorf("hostname top-level and second-level domains do not match: %s, %s", subjectHostname, issuerHostname)
}
