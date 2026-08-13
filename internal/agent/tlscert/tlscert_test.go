package tlscert

import (
	"crypto/x509"
	"strings"
	"testing"
	"time"
)

func TestGenerateLoadFingerprintStable(t *testing.T) {
	certPEM, keyPEM, err := Generate("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cert, err := Load(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fp1, err := SPKIFingerprint(cert)
	if err != nil {
		t.Fatalf("SPKIFingerprint: %v", err)
	}
	if !strings.HasPrefix(fp1, "sha256/") {
		t.Errorf("fingerprint format: %q", fp1)
	}

	// Simulate a reboot: reload the same PEM material — the pin must not move.
	cert2, err := Load(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	fp2, err := SPKIFingerprint(cert2)
	if err != nil {
		t.Fatalf("SPKIFingerprint reload: %v", err)
	}
	if fp1 != fp2 {
		t.Errorf("fingerprint changed across reload: %q != %q", fp1, fp2)
	}

	// A fresh keypair must produce a different pin.
	otherCert, otherKey, err := Generate("ffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatalf("Generate other: %v", err)
	}
	other, err := Load(otherCert, otherKey)
	if err != nil {
		t.Fatalf("Load other: %v", err)
	}
	fp3, err := SPKIFingerprint(other)
	if err != nil {
		t.Fatalf("SPKIFingerprint other: %v", err)
	}
	if fp3 == fp1 {
		t.Error("distinct keys produced identical SPKI fingerprints")
	}
}

func TestCertificateShape(t *testing.T) {
	agentID := "0123456789abcdef0123456789abcdef"
	certPEM, keyPEM, err := Generate(agentID)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	cert, err := Load(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if leaf.Subject.CommonName != agentID {
		t.Errorf("CN = %q, want agent id", leaf.Subject.CommonName)
	}
	found := false
	for _, name := range leaf.DNSNames {
		if name == "poolpilot-relay.local" {
			found = true
		}
	}
	if !found {
		t.Errorf("SANs missing poolpilot-relay.local: %v", leaf.DNSNames)
	}
	if remaining := time.Until(leaf.NotAfter); remaining < 9*365*24*time.Hour {
		t.Errorf("validity too short: expires %v", leaf.NotAfter)
	}
}
