package config

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTestCA(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "orbitron test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	path := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(path, pemBytes, 0644); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	return path
}

func TestTLSClientConfigDefaultIsNil(t *testing.T) {
	cfg, err := (TLSConfig{}).TLSClientConfig()
	if err != nil {
		t.Fatalf("TLSClientConfig: %v", err)
	}
	if cfg != nil {
		t.Fatalf("expected nil for empty policy, got %+v", cfg)
	}
}

func TestTLSClientConfigLoadsCAFile(t *testing.T) {
	caFile := writeTestCA(t)
	cfg, err := (TLSConfig{CAFile: caFile}).TLSClientConfig()
	if err != nil {
		t.Fatalf("TLSClientConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil TLS config")
	}
	if cfg.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify must remain false with a CA file")
	}
	if cfg.RootCAs == nil {
		t.Fatal("expected root pool to be set")
	}
	if len(cfg.RootCAs.Subjects()) == 0 {
		t.Fatal("expected at least one subject in the custom root pool")
	}
}

func TestTLSClientConfigRejectsMissingCAFile(t *testing.T) {
	_, err := (TLSConfig{CAFile: filepath.Join(t.TempDir(), "nope.crt")}).TLSClientConfig()
	if err == nil {
		t.Fatal("expected error for missing CA file")
	}
}

func TestTLSClientConfigRejectsJunkCAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "junk.crt")
	if err := os.WriteFile(path, []byte("not a pem"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := (TLSConfig{CAFile: path}).TLSClientConfig()
	if err == nil {
		t.Fatal("expected error for non-PEM CA file")
	}
}

func TestTLSClientConfigInsecureSkipVerify(t *testing.T) {
	cfg, err := (TLSConfig{InsecureSkipVerify: true}).TLSClientConfig()
	if err != nil {
		t.Fatalf("TLSClientConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil TLS config")
	}
	if !cfg.InsecureSkipVerify {
		t.Fatal("expected InsecureSkipVerify to be honored")
	}
	if cfg.RootCAs != nil {
		t.Fatal("expected no root pool for the skip-verify-only policy")
	}
}

func TestTLSGitEnvEmptyByDefault(t *testing.T) {
	if env := (TLSConfig{}).GitEnv(); env != nil {
		t.Fatalf("expected no git env, got %v", env)
	}
}

func TestTLSGitEnvCAFile(t *testing.T) {
	env := (TLSConfig{CAFile: "/etc/orbitron/ca.crt"}).GitEnv()
	if len(env) != 1 || env[0] != "GIT_SSL_CAINFO=/etc/orbitron/ca.crt" {
		t.Fatalf("unexpected git env: %v", env)
	}
}

func TestTLSGitEnvInsecure(t *testing.T) {
	env := (TLSConfig{InsecureSkipVerify: true}).GitEnv()
	if len(env) != 1 || env[0] != "GIT_SSL_NO_VERIFY=true" {
		t.Fatalf("unexpected git env: %v", env)
	}
}

func TestLoadConfigHonorsTLSPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	content := "tls:\n  ca_file: \"/etc/orbitron/ca.crt\"\n  insecure_skip_tls_verify: true\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.TLS.CAFile != "/etc/orbitron/ca.crt" {
		t.Fatalf("CAFile not honored: %+v", cfg.TLS)
	}
	if !cfg.TLS.InsecureSkipVerify {
		t.Fatalf("InsecureSkipVerify not honored: %+v", cfg.TLS)
	}
	if !cfg.TLS.On() {
		t.Fatal("expected On() to be true")
	}
}
