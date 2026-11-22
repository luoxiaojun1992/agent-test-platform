// Package pki validates the public CA certificates that validation MySQL registrations and Database Profiles
// may carry. A certificate is public material kept in Platform MySQL, not a credential.
package pki

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
)

// MaxCAPEMBytes bounds the optional CA certificate text of one registration or profile.
const MaxCAPEMBytes = 16 * 1024

// ErrInvalidCA reports CA text that is too large, is not X.509 PEM, or holds anything but certificates.
var ErrInvalidCA = errors.New("ca_certificate_pem must hold X.509 certificates of at most 16384 bytes")

// ValidateCAPEM accepts one or more PEM CERTIFICATE blocks that parse as X.509, separated only by
// whitespace. It never inspects trust or expiry: whether the certificate verifies a server is decided when
// the connection is made.
func ValidateCAPEM(text string) error {
	if len(text) > MaxCAPEMBytes {
		return ErrInvalidCA
	}
	rest := []byte(text)
	found := false
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return ErrInvalidCA
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return ErrInvalidCA
		}
		found = true
	}
	if !found || len(bytes.TrimSpace(rest)) > 0 {
		return ErrInvalidCA
	}
	return nil
}
