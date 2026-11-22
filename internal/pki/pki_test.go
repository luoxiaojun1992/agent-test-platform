package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"
)

func certificatePEM(t *testing.T, name string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestValidateCAPEMAcceptsOneOrMoreCertificates(t *testing.T) {
	t.Parallel()

	one := certificatePEM(t, "root")
	if err := ValidateCAPEM(one); err != nil {
		t.Fatalf("one certificate: %v", err)
	}
	if err := ValidateCAPEM(one + "\n" + certificatePEM(t, "intermediate") + "\n"); err != nil {
		t.Fatalf("two certificates: %v", err)
	}
}

func TestValidateCAPEMRejectsEverythingElse(t *testing.T) {
	t.Parallel()

	valid := certificatePEM(t, "root")
	tests := map[string]string{
		"empty":            "",
		"not PEM":          "not a certificate",
		"private key":      string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")})),
		"garbled DER":      "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
		"trailing text":    valid + "stray text",
		"oversized":        valid + strings.Repeat("\n", MaxCAPEMBytes),
		"key after a cert": valid + string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")})),
	}
	for name, text := range tests {
		if err := ValidateCAPEM(text); !errors.Is(err, ErrInvalidCA) {
			t.Errorf("%s: ValidateCAPEM = %v, want ErrInvalidCA", name, err)
		}
	}
}
