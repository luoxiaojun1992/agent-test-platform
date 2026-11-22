package sandbox

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func testCertificatePEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2036, 1, 1, 0, 0, 0, 0, time.UTC),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestApplyTLSVerifyIdentityUsesSystemRootsWithoutACertificate(t *testing.T) {
	t.Parallel()

	config := mysql.NewConfig()
	if err := ApplyTLS(config, TLSVerifyIdentity, "target.internal", ""); err != nil {
		t.Fatal(err)
	}
	if config.TLS == nil || config.TLS.ServerName != "target.internal" || config.TLS.MinVersion != tls.VersionTLS12 || config.TLS.RootCAs != nil {
		t.Fatalf("TLS = %#v", config.TLS)
	}
}

func TestApplyTLSVerifyIdentityTrustsOnlyTheGivenCertificate(t *testing.T) {
	t.Parallel()

	config := mysql.NewConfig()
	if err := ApplyTLS(config, TLSVerifyIdentity, "target.internal", testCertificatePEM(t)); err != nil {
		t.Fatal(err)
	}
	if config.TLS == nil || config.TLS.RootCAs == nil || len(config.TLS.RootCAs.Subjects()) != 1 { //nolint:staticcheck // counts the pool's certificates
		t.Fatalf("TLS = %#v", config.TLS)
	}
}

func TestApplyTLSDisabledAndInvalidInputs(t *testing.T) {
	t.Parallel()

	disabled := mysql.NewConfig()
	if err := ApplyTLS(disabled, TLSDisabledLocalOnly, "target.internal", ""); err != nil || disabled.TLSConfig != "false" || disabled.TLS != nil {
		t.Fatalf("disabled config = %#v, %v", disabled, err)
	}
	if err := ApplyTLS(mysql.NewConfig(), "PREFERRED", "target.internal", ""); !errors.Is(err, ErrTLSConfig) {
		t.Fatalf("unknown mode = %v", err)
	}
	if err := ApplyTLS(mysql.NewConfig(), TLSVerifyIdentity, "target.internal", "not a certificate"); !errors.Is(err, ErrTLSConfig) {
		t.Fatalf("garbage CA = %v", err)
	}
}

func TestOpenDatabaseClonesBaseConfigAndKeepsTLS(t *testing.T) {
	t.Parallel()

	base := mysql.NewConfig()
	base.User, base.Net, base.Addr = "atp_admin", "tcp", "target.internal:3306"
	if err := ApplyTLS(base, TLSVerifyIdentity, "target.internal", testCertificatePEM(t)); err != nil {
		t.Fatal(err)
	}
	connection := AdminConnection{Base: base}
	db, err := connection.OpenDatabase(t.Context(), "dataset_abc")
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if base.DBName != "" {
		t.Fatalf("OpenDatabase changed the shared base config: DBName = %q", base.DBName)
	}
}
