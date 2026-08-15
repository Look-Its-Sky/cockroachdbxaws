package otlpreceiver_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/model"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/otlpreceiver"
	"github.com/Look-Its-Sky/cockroachdbxaws/services/static-log-analysis/internal/testsupport/fakeclock"
)

func staticTrust() otlpreceiver.TrustConfig {
	return otlpreceiver.TrustConfig{
		Source: otlpreceiver.TrustSourceStaticLocal, SourceAccount: "aws-account-a", Region: "us-east-1",
		AllowedEnvironments: []string{"production"}, AllowedServices: []string{"paymentservice"},
		SourceInstance: "collector-a", CredentialIdentity: "workload-a",
	}
}

func mutualTrust() otlpreceiver.TrustConfig {
	trust := staticTrust()
	trust.Source = otlpreceiver.TrustSourceMutualTLS
	trust.SourceInstance = ""
	trust.CredentialIdentity = ""
	trust.TLS = otlpreceiver.TLSConfig{CertificateFile: "/tls/server.crt", KeyFile: "/tls/server.key", ClientCAFile: "/tls/ca.crt"}
	return trust
}

func TestStaticLocalEnvelopeComesEntirelyFromConfiguration(t *testing.T) {
	authenticator, err := otlpreceiver.NewAuthenticator(staticTrust())
	if err != nil {
		t.Fatal(err)
	}
	received := fakeclock.Origin
	envelope, err := authenticator.Envelope(otlpreceiver.Peer{RemoteAddr: "10.0.0.7:55123"}, received)
	if err != nil {
		t.Fatal(err)
	}
	want := model.TrustedEnvelope{
		SourceType: model.SourceTypeOTLP, SourceAccount: "aws-account-a", Region: "us-east-1",
		AllowedEnvironments: []string{"production"}, AllowedServices: []string{"paymentservice"},
		SourceInstance: "collector-a", CredentialIdentity: "workload-a", ReceivedAt: received,
	}
	if !reflect.DeepEqual(envelope, want) {
		t.Fatalf("envelope=%+v want=%+v", envelope, want)
	}
}

// The returned envelope carries slices. If they aliased the receiver's own
// configuration, one admitted batch could rewrite the allowed set every later
// batch is checked against.
func TestEnvelopeSlicesAreNotSharedWithTheReceiverConfiguration(t *testing.T) {
	authenticator, err := otlpreceiver.NewAuthenticator(staticTrust())
	if err != nil {
		t.Fatal(err)
	}
	first, err := authenticator.Envelope(otlpreceiver.Peer{}, fakeclock.Origin)
	if err != nil {
		t.Fatal(err)
	}
	first.AllowedServices[0] = "attacker-service"
	first.AllowedEnvironments[0] = "attacker-environment"
	second, err := authenticator.Envelope(otlpreceiver.Peer{}, fakeclock.Origin)
	if err != nil {
		t.Fatal(err)
	}
	if second.AllowedServices[0] != "paymentservice" || second.AllowedEnvironments[0] != "production" {
		t.Fatalf("a mutated envelope changed the next one: %+v", second)
	}
}

func TestUnknownTrustSourceIsRefused(t *testing.T) {
	trust := staticTrust()
	trust.Source = "payload"
	if _, err := otlpreceiver.NewAuthenticator(trust); !errors.Is(err, otlpreceiver.ErrInvalidTrustConfig) {
		t.Fatalf("a trust source outside the closed set was accepted: %v", err)
	}
	if _, err := otlpreceiver.NewAuthenticator(otlpreceiver.TrustConfig{}); !errors.Is(err, otlpreceiver.ErrInvalidTrustConfig) {
		t.Fatalf("an unset trust source was accepted: %v", err)
	}
}

func TestStaticLocalTrustRequiresACompleteEnvelope(t *testing.T) {
	for name, mutate := range map[string]func(*otlpreceiver.TrustConfig){
		"no account":     func(c *otlpreceiver.TrustConfig) { c.SourceAccount = "" },
		"no region":      func(c *otlpreceiver.TrustConfig) { c.Region = "" },
		"no environment": func(c *otlpreceiver.TrustConfig) { c.AllowedEnvironments = nil },
		"no service":     func(c *otlpreceiver.TrustConfig) { c.AllowedServices = nil },
		"no instance":    func(c *otlpreceiver.TrustConfig) { c.SourceInstance = "" },
		"no credential":  func(c *otlpreceiver.TrustConfig) { c.CredentialIdentity = "" },
		"blank service":  func(c *otlpreceiver.TrustConfig) { c.AllowedServices = []string{"  "} },
	} {
		t.Run(name, func(t *testing.T) {
			trust := staticTrust()
			mutate(&trust)
			if _, err := otlpreceiver.NewAuthenticator(trust); err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// Under mutual_tls the caller's identity is whatever the verified certificate
// says, never what the operator or the payload says.
func TestMutualTLSDerivesCallerIdentityFromTheVerifiedCertificate(t *testing.T) {
	authenticator, err := otlpreceiver.NewAuthenticator(mutualTrust())
	if err != nil {
		t.Fatal(err)
	}
	state := verifiedPeer(t, "collector-a.otel.svc", "spiffe://cluster.local/ns/otel/sa/collector")
	envelope, err := authenticator.Envelope(otlpreceiver.Peer{TLS: state}, fakeclock.Origin)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.SourceInstance != "collector-a.otel.svc" {
		t.Fatalf("source instance=%q, not the certificate's DNS identity", envelope.SourceInstance)
	}
	if envelope.CredentialIdentity != "spiffe://cluster.local/ns/otel/sa/collector" {
		t.Fatalf("credential identity=%q, not the certificate's URI identity", envelope.CredentialIdentity)
	}
	if envelope.Region != "us-east-1" || envelope.SourceAccount != "aws-account-a" {
		t.Fatalf("the configured regional boundary was not preserved: %+v", envelope)
	}
}

func TestMutualTLSRefusesAPeerItCouldNotVerify(t *testing.T) {
	authenticator, err := otlpreceiver.NewAuthenticator(mutualTrust())
	if err != nil {
		t.Fatal(err)
	}
	// Each case differs from an accepted peer in exactly one way, so each guard
	// is the only thing rejecting its own case.
	usable := verifiedPeer(t, "collector-a.otel.svc", "spiffe://cluster.local/ns/otel/sa/collector")
	unverified := *usable
	unverified.VerifiedChains = nil
	unverified.PeerCertificates = usable.VerifiedChains[0]
	incomplete := *usable
	incomplete.HandshakeComplete = false
	cases := map[string]otlpreceiver.Peer{
		"plaintext": {RemoteAddr: "10.0.0.7:1234"},
		// A certificate the caller simply presented, which no configured CA
		// vouched for, is not an identity.
		"presented but unverified": {TLS: &unverified},
		"handshake not complete":   {TLS: &incomplete},
	}
	for name, peer := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := authenticator.Envelope(peer, fakeclock.Origin); !errors.Is(err, otlpreceiver.ErrUnauthenticated) {
				t.Fatalf("%s produced an envelope: %v", name, err)
			}
		})
	}
}

// A certificate with no stable machine identity cannot pair with a record UID
// to make identity unforgeable across sources, so it is not a usable caller.
func TestMutualTLSRefusesACertificateWithoutAStableIdentity(t *testing.T) {
	authenticator, err := otlpreceiver.NewAuthenticator(mutualTrust())
	if err != nil {
		t.Fatal(err)
	}
	state := verifiedPeer(t, "", "")
	if _, err := authenticator.Envelope(otlpreceiver.Peer{TLS: state}, fakeclock.Origin); !errors.Is(err, otlpreceiver.ErrUnauthenticated) {
		t.Fatalf("an anonymous certificate produced an envelope: %v", err)
	}
}

func TestMutualTLSRefusesOperatorDeclaredCallerIdentity(t *testing.T) {
	withInstance := mutualTrust()
	withInstance.SourceInstance = "collector-a"
	if _, err := otlpreceiver.NewAuthenticator(withInstance); !errors.Is(err, otlpreceiver.ErrInvalidTrustConfig) {
		t.Fatalf("an operator-declared source instance was accepted: %v", err)
	}
	withCredential := mutualTrust()
	withCredential.CredentialIdentity = "workload-a"
	if _, err := otlpreceiver.NewAuthenticator(withCredential); !errors.Is(err, otlpreceiver.ErrInvalidTrustConfig) {
		t.Fatalf("an operator-declared credential identity was accepted: %v", err)
	}
}

// verifiedPeer builds the connection state a completed mutually authenticated
// handshake leaves behind. The certificate is generated here rather than
// checked in so no test fixture ever expires.
func verifiedPeer(t *testing.T, dnsName, uri string) *tls.ConnectionState {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "collector"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if dnsName != "" {
		template.DNSNames = []string{dnsName}
	}
	if uri != "" {
		parsed, parseErr := url.Parse(uri)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		template.URIs = append(template.URIs, parsed)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.ConnectionState{HandshakeComplete: true, VerifiedChains: [][]*x509.Certificate{{certificate}}}
}
