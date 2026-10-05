package edc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const responsivenessConfigVersion = 1

var (
	errQualityConfigStatus     = errors.New("responsiveness config request failed")
	errQualityConfigTooLarge   = errors.New("responsiveness config is too large")
	errQualityConfigJSON       = errors.New("responsiveness config is not valid JSON")
	errQualityConfigVersion    = errors.New("responsiveness config has an unsupported version")
	errQualityConfigMissingURL = errors.New("responsiveness config misses a URL")
	errQualityConfigMixedHosts = errors.New("responsiveness config URLs use different hosts")
	errQualityServerInvalid    = errors.New("must be an absolute http or https URL with a host and no user info")
)

type responsivenessConfig struct {
	LargeDownloadURL string
	SmallDownloadURL string
	UploadURL        string
	TestEndpoint     string
}

type responsivenessConfigDocument struct {
	Version      int               `json:"version"`
	URLs         map[string]string `json:"urls"`
	TestEndpoint string            `json:"test_endpoint"`
}

// Apple 서버는 draft 이름과 *_https_* 이름을 함께 준다. HTTP/2 협상을 위해 https 이름을 먼저 본다.
var (
	largeDownloadURLKeys = []string{"large_https_download_url", "large_download_url"}
	smallDownloadURLKeys = []string{"small_https_download_url", "small_download_url"}
	uploadURLKeys        = []string{"https_upload_url", "upload_url"}
)

func validateQualityServer(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
		return fmt.Errorf("%w: %q", errQualityServerInvalid, raw)
	}
	return nil
}

// qualityReportURL은 query와 fragment를 뺀다. token이 들어 있을 수 있는데 --redact는 URL 안쪽을 지우지 않는다.
func qualityReportURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	parsed.RawQuery, parsed.ForceQuery, parsed.Fragment, parsed.RawFragment = "", false, "", ""
	return parsed.String()
}

func fetchResponsivenessConfig(ctx context.Context, client *http.Client, configURL string) (responsivenessConfig, error) {
	if err := validateQualityServer(configURL); err != nil {
		return responsivenessConfig{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, configURL, nil)
	if err != nil {
		return responsivenessConfig{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return responsivenessConfig{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return responsivenessConfig{}, fmt.Errorf("%w: %s", errQualityConfigStatus, response.Status)
	}
	return parseResponsivenessConfig(response.Body)
}

func parseResponsivenessConfig(body io.Reader) (responsivenessConfig, error) {
	data, err := io.ReadAll(io.LimitReader(body, configBodyLimit+1))
	if err != nil {
		return responsivenessConfig{}, err
	}
	if len(data) > configBodyLimit {
		return responsivenessConfig{}, fmt.Errorf("%w: more than %d bytes", errQualityConfigTooLarge, configBodyLimit)
	}
	var document responsivenessConfigDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return responsivenessConfig{}, fmt.Errorf("%w: %v", errQualityConfigJSON, err)
	}
	if document.Version != responsivenessConfigVersion {
		return responsivenessConfig{}, fmt.Errorf("%w: %d", errQualityConfigVersion, document.Version)
	}
	config := responsivenessConfig{TestEndpoint: strings.TrimSpace(document.TestEndpoint)}
	targets := []struct {
		keys   []string
		target *string
	}{
		{largeDownloadURLKeys, &config.LargeDownloadURL},
		{smallDownloadURLKeys, &config.SmallDownloadURL},
		{uploadURLKeys, &config.UploadURL},
	}
	host := ""
	for _, entry := range targets {
		value := firstConfigURL(document.URLs, entry.keys)
		if value == "" {
			return responsivenessConfig{}, fmt.Errorf("%w: %s", errQualityConfigMissingURL, entry.keys[len(entry.keys)-1])
		}
		if err := validateQualityServer(value); err != nil {
			return responsivenessConfig{}, err
		}
		parsed, _ := url.Parse(value)
		if host == "" {
			host = parsed.Host
		} else if !strings.EqualFold(host, parsed.Host) {
			return responsivenessConfig{}, fmt.Errorf("%w: %s and %s", errQualityConfigMixedHosts, host, parsed.Host)
		}
		*entry.target = value
	}
	return config, nil
}

func firstConfigURL(urls map[string]string, keys []string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(urls[key]); value != "" {
			return value
		}
	}
	return ""
}
