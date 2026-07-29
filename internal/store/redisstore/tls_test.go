// SPDX-License-Identifier: Apache-2.0

package redisstore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A private CA is the common case for managed and self-hosted Redis. Without
// TLSCAFile the only way to use TLS at all would be to stop verifying it.
func TestBuildTLS(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	junk := filepath.Join(dir, "junk.pem")
	require.NoError(t, os.WriteFile(ca, caPEM(t), 0o600))
	require.NoError(t, os.WriteFile(junk, []byte("not a certificate"), 0o600))

	t.Run("disabled yields no config", func(t *testing.T) {
		t.Parallel()
		c, err := buildTLS(Config{TLSEnabled: false, TLSCAFile: ca})
		require.NoError(t, err)
		require.Nil(t, c)
	})

	t.Run("enabled without a CA uses the system roots", func(t *testing.T) {
		t.Parallel()
		c, err := buildTLS(Config{TLSEnabled: true})
		require.NoError(t, err)
		require.NotNil(t, c)
		require.Nil(t, c.RootCAs)
		require.False(t, c.InsecureSkipVerify)
		require.EqualValues(t, 771, c.MinVersion, "TLS 1.2 floor")
	})

	t.Run("a CA file is trusted", func(t *testing.T) {
		t.Parallel()
		c, err := buildTLS(Config{TLSEnabled: true, TLSCAFile: ca})
		require.NoError(t, err)
		require.NotNil(t, c.RootCAs)
	})

	t.Run("a missing CA file is an error, not a silent fallback", func(t *testing.T) {
		t.Parallel()
		_, err := buildTLS(Config{TLSEnabled: true, TLSCAFile: filepath.Join(dir, "absent.pem")})
		require.Error(t, err)
		require.Contains(t, err.Error(), "REDIS_TLS_CA_FILE")
	})

	t.Run("a CA file with no certificate in it is an error", func(t *testing.T) {
		t.Parallel()
		_, err := buildTLS(Config{TLSEnabled: true, TLSCAFile: junk})
		require.Error(t, err)
		require.Contains(t, err.Error(), "no PEM certificate")
	})

	t.Run("a client certificate without its key is rejected", func(t *testing.T) {
		t.Parallel()
		_, err := buildTLS(Config{TLSEnabled: true, TLSCertFile: ca})
		require.Error(t, err)
		require.Contains(t, err.Error(), "must be set together")

		_, err = buildTLS(Config{TLSEnabled: true, TLSKeyFile: ca})
		require.Error(t, err)
		require.Contains(t, err.Error(), "must be set together")
	})

	t.Run("skip verify is honoured", func(t *testing.T) {
		t.Parallel()
		c, err := buildTLS(Config{TLSEnabled: true, TLSSkipVerify: true})
		require.NoError(t, err)
		require.True(t, c.InsecureSkipVerify)
	})
}

// caPEM generates a real self-signed CA at test time, so nothing here can expire or
// be mistyped.
func caPEM(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "rig-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
