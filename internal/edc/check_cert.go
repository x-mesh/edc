package edc

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// check.cert는 이 호스트에 놓인 인증서 파일의 만료다. tls와 doctor는 접속해서 받은 인증서만 보므로, 아직 서비스가
// 읽지 않았거나 외부에 열려 있지 않은 인증서(kubelet, etcd, 내부 nginx)는 여기서만 보인다.

// checkCertLimit은 한 번에 읽는 파일 수의 상한이다. 설정한 glob이 넓어도 check가 느려지지 않게 한다.
const checkCertLimit = 500

// checkCertMaxSize보다 큰 파일은 인증서로 보지 않는다. 인증서 묶음도 수백 KB를 넘지 않는다.
const checkCertMaxSize = 1 << 20

// checkCertDefaults는 서비스가 쓰는 인증서가 놓이는 알려진 위치다. CA 묶음(/etc/ssl/certs)은 만료된 루트가 늘 섞여 있어 뺀다.
var checkCertDefaults = []string{
	"/etc/letsencrypt/live/*/cert.pem",
	"/etc/kubernetes/pki/*.crt",
	"/etc/kubernetes/pki/etcd/*.crt",
	"/var/lib/kubelet/pki/kubelet.crt",
	"/var/lib/kubelet/pki/kubelet-client-current.pem",
}

// checkCertConfigs는 ssl_certificate나 SSLCertificateFile로 인증서 경로를 적는 웹 서버 설정이다.
var checkCertConfigs = []string{
	"/etc/nginx/nginx.conf", "/etc/nginx/conf.d/*.conf", "/etc/nginx/sites-enabled/*",
	"/opt/homebrew/etc/nginx/nginx.conf", "/opt/homebrew/etc/nginx/servers/*", "/usr/local/etc/nginx/nginx.conf", "/usr/local/etc/nginx/servers/*",
	"/etc/apache2/sites-enabled/*", "/etc/httpd/conf.d/*.conf",
}

// checkCertDirective는 설정 줄 하나의 인증서 경로다. $변수가 들어간 경로는 실행할 때 정해지므로 읽지 않는다.
var checkCertDirective = regexp.MustCompile(`^\s*(?:ssl_certificate|SSLCertificateFile)\s+["']?([^\s;"'$]+)["']?\s*;?\s*$`)

type checkCertFile struct {
	Path, Subject string
	NotAfter      time.Time
}

type checkCerts struct {
	Files []checkCertFile
	// Unreadable는 권한 때문에 읽지 못한 파일이다. 판정하지 않고 수만 알린다. glob이 key 파일도 고르므로 인증서가 아닌
	// 파일은 조용히 건너뛴다.
	Unreadable []string
	// Truncated는 checkCertLimit에서 멈췄다는 것이다.
	Truncated bool
}

// readCheckCerts는 기본 위치, 웹 서버 설정이 가리키는 파일, 설정의 cert_paths를 한 번씩 읽는다.
func readCheckCerts(extra []string) checkCerts {
	var paths []string
	for _, pattern := range append(slices.Clone(checkCertDefaults), extra...) {
		matches, _ := filepath.Glob(pattern)
		paths = append(paths, matches...)
	}
	for _, pattern := range checkCertConfigs {
		matches, _ := filepath.Glob(pattern)
		for _, config := range matches {
			paths = append(paths, checkCertConfigPaths(config)...)
		}
	}
	sort.Strings(paths)
	paths = slices.Compact(paths)
	var certs checkCerts
	if len(paths) > checkCertLimit {
		paths, certs.Truncated = paths[:checkCertLimit], true
	}
	for _, path := range paths {
		file, err := readCheckCertFile(path)
		switch {
		case errors.Is(err, fs.ErrPermission):
			certs.Unreadable = append(certs.Unreadable, path)
		case err == nil:
			certs.Files = append(certs.Files, file)
		}
	}
	return certs
}

func checkCertConfigPaths(config string) []string {
	data, err := os.ReadFile(config)
	if err != nil || len(data) > checkCertMaxSize {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(string(data), "\n") {
		if match := checkCertDirective.FindStringSubmatch(line); match != nil {
			path := match[1]
			// Apache와 nginx는 상대 경로를 서버 root 기준으로 읽는다. root를 모르므로 절대 경로만 쓴다.
			if filepath.IsAbs(path) {
				paths = append(paths, path)
			}
		}
	}
	return paths
}

var errCheckNotCert = errors.New("no certificate")

// readCheckCertFile은 파일의 첫 인증서를 읽는다. fullchain처럼 묶인 파일은 서버 인증서가 맨 앞에 온다.
func readCheckCertFile(path string) (checkCertFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return checkCertFile{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > checkCertMaxSize {
		return checkCertFile{}, errCheckNotCert
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return checkCertFile{}, err
	}
	certificate, err := parseFirstCertificate(data)
	if err != nil {
		return checkCertFile{}, errCheckNotCert
	}
	return checkCertFile{Path: path, Subject: certificate.Subject.CommonName, NotAfter: certificate.NotAfter}, nil
}

// parseFirstCertificate는 PEM이면 첫 CERTIFICATE 블록을, 아니면 DER 하나를 읽는다. kubelet과 일부 CA는 key와 인증서를 한
// 파일에 두므로 다른 블록은 건너뛴다.
func parseFirstCertificate(data []byte) (*x509.Certificate, error) {
	for rest := data; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
	}
	return x509.ParseCertificate(data)
}

func checkCertResult(facts checkFacts) Result {
	const id = "cert"
	certs := facts.Certs
	if len(certs.Files) == 0 {
		if len(certs.Unreadable) > 0 {
			return checkSkip(id, checkNames(certs.Unreadable), fs.ErrPermission)
		}
		return checkResult(id, StatusSkip, T("check.cert.none"))
	}
	now := facts.Now
	if now.IsZero() {
		now = time.Now()
	}
	files := slices.Clone(certs.Files)
	slices.SortStableFunc(files, func(a, b checkCertFile) int { return a.NotAfter.Compare(b.NotAfter) })
	var issues checkIssues
	next := ""
	for _, file := range files {
		left := file.NotAfter.Sub(now)
		days := int(left.Hours() / 24)
		switch {
		case left <= 0:
			issues.add(StatusFail, T("check.cert.expired", file.Path, file.NotAfter.UTC().Format("2006-01-02")))
		case days < certificateWarnDays:
			issues.add(StatusWarn, T("check.cert.expiring", file.Path, days))
		default:
			continue
		}
		if next == "" {
			next = "openssl x509 -noout -subject -enddate -in " + quoteLocalShellWord(file.Path)
		}
	}
	first := files[0]
	result := issues.result(id, T("check.cert.ok", len(files), first.NotAfter.UTC().Format("2006-01-02"), first.Path), next)
	if len(certs.Unreadable) > 0 {
		result.Warnings = append(result.Warnings, T("check.cert.unreadable", len(certs.Unreadable), checkNames(certs.Unreadable)))
	}
	if certs.Truncated {
		result.Warnings = append(result.Warnings, T("check.cert.truncated", checkCertLimit))
	}
	return result
}
