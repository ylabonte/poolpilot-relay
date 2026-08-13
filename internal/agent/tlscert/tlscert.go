// Package tlscert manages the agent's self-signed LAN-API certificate. Trust
// is anchored via the SPKI fingerprint published in the mDNS TXT record and
// GET /v1/info — apps pin the key, not a CA chain, so the certificate only
// needs to be stable, not signed.
package tlscert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"time"
)

// Validity is deliberately long: the pinning model pins the SPKI, so rotating
// certificates buys nothing and would only break paired apps.
const Validity = 10 * 365 * 24 * time.Hour

// Generate mints a self-signed ECDSA P-256 certificate for the agent. SANs
// cover the mDNS host names; the agent ID is the CN so the cert is
// self-describing in debugging tools.
func Generate(agentID string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("tlscert: generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, fmt.Errorf("tlscert: serial: %w", err)
	}

	dnsNames := []string{"poolpilot-relay.local"}
	if hostname, herr := os.Hostname(); herr == nil && hostname != "" {
		dnsNames = append(dnsNames, hostname)
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: agentID},
		NotBefore:             now.Add(-time.Hour), // tolerate relay clock skew right after boot
		NotAfter:              now.Add(Validity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("tlscert: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("tlscert: marshal key: %w", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// Load parses persisted PEM material back into a serving certificate.
func Load(certPEM, keyPEM []byte) (tls.Certificate, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("tlscert: parse key pair: %w", err)
	}
	return cert, nil
}

// SPKIFingerprint returns the RFC 7469-style pin "sha256/<base64(SHA256(SPKI))>"
// for the mDNS TXT record and /v1/info.
func SPKIFingerprint(cert tls.Certificate) (string, error) {
	if len(cert.Certificate) == 0 {
		return "", fmt.Errorf("tlscert: certificate has no DER blocks")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return "", fmt.Errorf("tlscert: parse leaf: %w", err)
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return "sha256/" + base64.StdEncoding.EncodeToString(sum[:]), nil
}
