package edc

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
	"slices"
	"strings"
	"testing"
	"time"
)

func testCertificatePEM(t *testing.T, name string, notAfter time.Time) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: notAfter.Add(-365 * 24 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestParseFirstCertificateSkipsKeysAndReadsTheLeaf(t *testing.T) {
	expiry := time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC)
	leaf := testCertificatePEM(t, "leaf", expiry)
	issuer := testCertificatePEM(t, "issuer", expiry.AddDate(5, 0, 0))
	key := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("not a real key")})
	certificate, err := parseFirstCertificate(append(append(key, leaf...), issuer...))
	if err != nil || certificate.Subject.CommonName != "leaf" {
		t.Fatalf("certificate = %v, %v", certificate, err)
	}
	block, _ := pem.Decode(leaf)
	if certificate, err := parseFirstCertificate(block.Bytes); err != nil || !certificate.NotAfter.Equal(expiry) {
		t.Fatalf("DER = %v, %v", certificate, err)
	}
	if _, err := parseFirstCertificate(key); err == nil {
		t.Fatal("a key file must not parse as a certificate")
	}
}

func TestCheckCertConfigPathsReadsOnlyAbsoluteCertificateDirectives(t *testing.T) {
	config := filepath.Join(t.TempDir(), "site.conf")
	text := strings.Join([]string{
		"server {",
		"    ssl_certificate /etc/nginx/tls/site.crt;",
		"    ssl_certificate_key /etc/nginx/tls/site.key;",
		`    ssl_certificate "/etc/nginx/tls/quoted.crt";`,
		"    ssl_certificate $ssl_cert_path;",
		"    ssl_certificate tls/relative.crt;",
		"    # ssl_certificate /etc/nginx/tls/commented.crt;",
		"}",
		"SSLCertificateFile /etc/httpd/tls/apache.crt",
	}, "\n")
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	want := []string{"/etc/nginx/tls/site.crt", "/etc/nginx/tls/quoted.crt", "/etc/httpd/tls/apache.crt"}
	if got := checkCertConfigPaths(config); !slices.Equal(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

func TestReadCheckCertsFollowsConfigsAndCertPaths(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name string, data []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	served := write("served.crt", testCertificatePEM(t, "served", now.AddDate(0, 0, 90)))
	extra := write("extra.pem", testCertificatePEM(t, "extra", now.AddDate(0, 0, 10)))
	write("extra-key.pem", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("key")}))
	config := write("nginx.conf", []byte("ssl_certificate "+served+";\nssl_certificate "+served+";\n"))
	defaults, configs := checkCertDefaults, checkCertConfigs
	checkCertDefaults, checkCertConfigs = nil, []string{config}
	t.Cleanup(func() { checkCertDefaults, checkCertConfigs = defaults, configs })

	certs := readCheckCerts([]string{filepath.Join(dir, "extra*.pem")})
	var paths []string
	for _, file := range certs.Files {
		paths = append(paths, file.Path)
	}
	// 설정이 같은 파일을 두 번 가리켜도 한 번만 읽고, key 파일은 건너뛴다.
	if want := []string{extra, served}; !slices.Equal(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

func TestCheckCertJudgesExpiryAndPointsAtTheEarliestProblem(t *testing.T) {
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	certs := checkCerts{Files: []checkCertFile{
		{Path: "/etc/a.crt", NotAfter: now.AddDate(0, 0, 90)},
		{Path: "/etc/b.crt", NotAfter: now.AddDate(0, 0, 10)},
		{Path: "/etc/my certs/c.crt", NotAfter: now.AddDate(0, 0, -2)},
	}}
	result := checkCertResult(checkFacts{Certs: certs, Now: now})
	if result.Status != StatusFail || !strings.Contains(result.Summary, "/etc/my certs/c.crt expired on 2026-11-01") || !strings.Contains(result.Summary, "/etc/b.crt expires in 10 days") {
		t.Fatalf("result = %+v", result)
	}
	if result.Next != "openssl x509 -noout -subject -enddate -in '/etc/my certs/c.crt'" {
		t.Fatalf("next = %q", result.Next)
	}
	healthy := checkCertResult(checkFacts{Certs: checkCerts{Files: certs.Files[:1], Unreadable: []string{"/etc/kubernetes/pki/ca.crt"}}, Now: now})
	if healthy.Status != StatusPass || healthy.Summary != T("check.cert.ok", 1, "2027-02-01", "/etc/a.crt") || len(healthy.Warnings) != 1 {
		t.Fatalf("healthy = %+v", healthy)
	}
	// 30일 경계는 tls와 같다.
	edge := checkCertResult(checkFacts{Certs: checkCerts{Files: []checkCertFile{{Path: "/e.crt", NotAfter: now.AddDate(0, 0, 30)}}}, Now: now})
	if edge.Status != StatusPass {
		t.Fatalf("30 days = %+v", edge)
	}
}

func TestCheckCertSkipsWithoutReadableFiles(t *testing.T) {
	if result := checkCertResult(checkFacts{}); result.Status != StatusSkip || result.Summary != T("check.cert.none") {
		t.Fatalf("none = %+v", result)
	}
	locked := checkCertResult(checkFacts{Certs: checkCerts{Unreadable: []string{"/etc/kubernetes/pki/apiserver.crt"}}})
	if locked.Status != StatusSkip || !strings.Contains(locked.Summary, "apiserver.crt") {
		t.Fatalf("locked = %+v", locked)
	}
}

func TestValidateCertPathsNeedsAbsoluteGlobs(t *testing.T) {
	if err := validateCertPaths("check", []string{"/etc/app/*.crt"}); err != nil {
		t.Fatal(err)
	}
	for _, pattern := range []string{"tls/*.crt", "/etc/[app/*.crt"} {
		if err := validateCertPaths("check", []string{pattern}); err == nil {
			t.Errorf("%q must be rejected", pattern)
		}
	}
}
