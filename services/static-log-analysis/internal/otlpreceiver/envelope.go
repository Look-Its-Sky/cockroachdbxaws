package otlpreceiver

import (
	"crypto/tls"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
)

var (
	ErrInvalidTrustConfig = errors.New("otlpreceiver: invalid trust configuration")
	// ErrUnauthenticated means the transport could not establish who is calling.
	// It is never a partial or record-local outcome: without a caller identity
	// there is no envelope, and without an envelope nothing may be admitted.
	ErrUnauthenticated = errors.New("otlpreceiver: caller is not authenticated")
)

// TrustSource names where a caller's authenticated identity comes from. The set
// is closed on purpose. security.md makes service, environment, and region
// attributes inside application logs untrusted, so there is deliberately no
// member meaning "from the payload" and no way to add one through configuration.
type TrustSource string

const (
	// TrustSourceMutualTLS authenticates the Collector with a client
	// certificate. The envelope's caller identity is read from the verified
	// chain; the operator only supplies the regional boundary it may claim.
	TrustSourceMutualTLS TrustSource = "mutual_tls"
	// TrustSourceStaticLocal authenticates nothing. Every caller that can reach
	// the socket receives the one configured envelope. It exists for local
	// development and MUST be selected explicitly, so an unauthenticated
	// receiver is always something an operator chose rather than defaulted into.
	TrustSourceStaticLocal TrustSource = "static_local"
)

// TrustConfig is the operator-supplied half of the trusted envelope. It never
// contains anything a request body could influence.
type TrustConfig struct {
	Source        TrustSource
	SourceAccount string
	// Region is the replica's regional boundary. An envelope whose region is not
	// the coordinator's scope is rejected permanently one layer down.
	Region              string
	AllowedEnvironments []string
	AllowedServices     []string
	// SourceInstance and CredentialIdentity describe the caller. Under
	// mutual_tls both are derived from the certificate and MUST be empty here.
	SourceInstance     string
	CredentialIdentity string
	// TLS carries the server's own material and the CA that client certificates
	// are verified against. It is required by, and only by, mutual_tls.
	TLS TLSConfig
}

type TLSConfig struct {
	CertificateFile string
	KeyFile         string
	ClientCAFile    string
}

func (t TLSConfig) empty() bool {
	return t.CertificateFile == "" && t.KeyFile == "" && t.ClientCAFile == ""
}

// Peer is everything a receiver may learn about a caller. It carries transport
// facts only: no request body is reachable from here, so no implementation of
// Authenticator can derive an envelope from application-supplied attributes
// even by mistake.
type Peer struct {
	TLS        *tls.ConnectionState
	RemoteAddr string
}

// Authenticator turns a transport peer into the trusted envelope a batch is
// admitted under.
type Authenticator interface {
	Envelope(peer Peer, receivedAt time.Time) (model.TrustedEnvelope, error)
	// RequiresClientCertificate reports whether the listener must be built so a
	// peer without a verified certificate never reaches a handler.
	RequiresClientCertificate() bool
}

func NewAuthenticator(config TrustConfig) (Authenticator, error) {
	if err := validateCommon(config); err != nil {
		return nil, err
	}
	switch config.Source {
	case TrustSourceStaticLocal:
		if strings.TrimSpace(config.SourceInstance) == "" || strings.TrimSpace(config.CredentialIdentity) == "" {
			return nil, fmt.Errorf("%w: static_local needs a source instance and credential identity, because nothing else establishes one", ErrInvalidTrustConfig)
		}
		if !config.TLS.empty() {
			return nil, fmt.Errorf("%w: static_local verifies no certificate, so TLS material would only look like protection", ErrInvalidTrustConfig)
		}
		return &staticAuthenticator{config: config}, nil
	case TrustSourceMutualTLS:
		if config.SourceInstance != "" || config.CredentialIdentity != "" {
			return nil, fmt.Errorf("%w: mutual_tls reads the caller from its certificate, so an operator-declared identity would be a second, disagreeing truth", ErrInvalidTrustConfig)
		}
		if config.TLS.CertificateFile == "" || config.TLS.KeyFile == "" || config.TLS.ClientCAFile == "" {
			return nil, fmt.Errorf("%w: mutual_tls needs a server certificate, key, and the client CA it verifies against", ErrInvalidTrustConfig)
		}
		return &mutualTLSAuthenticator{config: config}, nil
	default:
		return nil, fmt.Errorf("%w: trust source %q is not one of %q or %q", ErrInvalidTrustConfig, config.Source, TrustSourceMutualTLS, TrustSourceStaticLocal)
	}
}

func validateCommon(config TrustConfig) error {
	if strings.TrimSpace(config.SourceAccount) == "" || strings.TrimSpace(config.Region) == "" {
		return fmt.Errorf("%w: a source account and region are required", ErrInvalidTrustConfig)
	}
	if len(config.AllowedEnvironments) == 0 || len(config.AllowedServices) == 0 {
		return fmt.Errorf("%w: an empty allowed set would admit no record at all", ErrInvalidTrustConfig)
	}
	for _, value := range append(append([]string{}, config.AllowedEnvironments...), config.AllowedServices...) {
		if strings.TrimSpace(value) != value || value == "" {
			return fmt.Errorf("%w: allowed identities must be exact, unpadded values", ErrInvalidTrustConfig)
		}
	}
	return nil
}

type staticAuthenticator struct{ config TrustConfig }

func (a *staticAuthenticator) RequiresClientCertificate() bool { return false }

func (a *staticAuthenticator) Envelope(_ Peer, receivedAt time.Time) (model.TrustedEnvelope, error) {
	return a.config.envelope(a.config.SourceInstance, a.config.CredentialIdentity, receivedAt), nil
}

type mutualTLSAuthenticator struct{ config TrustConfig }

func (a *mutualTLSAuthenticator) RequiresClientCertificate() bool { return true }

func (a *mutualTLSAuthenticator) Envelope(peer Peer, receivedAt time.Time) (model.TrustedEnvelope, error) {
	// VerifiedChains is populated only by a handshake that actually verified the
	// client against the configured CA. Reading PeerCertificates instead would
	// accept any self-signed certificate a caller chose to present.
	if peer.TLS == nil || !peer.TLS.HandshakeComplete || len(peer.TLS.VerifiedChains) == 0 || len(peer.TLS.VerifiedChains[0]) == 0 {
		return model.TrustedEnvelope{}, fmt.Errorf("%w: no verified client certificate", ErrUnauthenticated)
	}
	leaf := peer.TLS.VerifiedChains[0][0]
	instance := ""
	if len(leaf.DNSNames) > 0 {
		instance = leaf.DNSNames[0]
	}
	credential := ""
	if len(leaf.URIs) > 0 {
		credential = leaf.URIs[0].String()
	}
	// The subject alternative names are the machine identity. A certificate with
	// neither cannot be paired with a record UID to make identity unforgeable
	// across sources, so it is not a caller this receiver can describe.
	if instance == "" || credential == "" {
		return model.TrustedEnvelope{}, fmt.Errorf("%w: the client certificate carries no DNS and URI identity", ErrUnauthenticated)
	}
	return a.config.envelope(instance, credential, receivedAt), nil
}

// envelope copies the allowed sets on every call. The returned envelope travels
// into normalization, which clones it again per record; sharing the receiver's
// backing arrays would let one request's mutation change the set every later
// request is authorized against.
func (t TrustConfig) envelope(instance, credential string, receivedAt time.Time) model.TrustedEnvelope {
	return model.TrustedEnvelope{
		SourceType:          model.SourceTypeOTLP,
		SourceAccount:       t.SourceAccount,
		Region:              t.Region,
		AllowedEnvironments: append([]string(nil), t.AllowedEnvironments...),
		AllowedServices:     append([]string(nil), t.AllowedServices...),
		SourceInstance:      instance,
		CredentialIdentity:  credential,
		ReceivedAt:          receivedAt.UTC(),
	}
}
