package edc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
)

// notify는 check와 jobs의 결과를 webhook으로 보낸다. 받는 쪽마다 형식이 달라 json, slack, discord 세 가지를 만든다.
// slack 형식은 Mattermost와 Rocket.Chat의 incoming webhook도 받는다.

type notifyConfig struct {
	Webhooks []notifyWebhook `yaml:"webhooks,omitempty" toml:"webhooks,omitempty"`
}

type notifyWebhook struct {
	Name   string `yaml:"name" toml:"name"`
	URL    string `yaml:"url" toml:"url"`
	Format string `yaml:"format,omitempty" toml:"format,omitempty"`
	// On은 보내는 기준이다. fail이 기본이고 warn은 warn 이상, always는 통과해도 보낸다.
	On string `yaml:"on,omitempty" toml:"on,omitempty"`
}

const (
	notifyTimeout = 10 * time.Second
	// notifyLines는 메시지에 적는 결과 수다. 나머지는 개수로 적는다.
	notifyLines = 10
	// notifyTextMax는 Discord content의 상한 2000자보다 작게 잡은 길이다.
	notifyTextMax = 1900
)

var (
	notifyFormats = []string{"json", "slack", "discord"}
	notifyLevels  = []string{"fail", "warn", "always"}
)

func validateNotifyConfig(config notifyConfig) error {
	names := map[string]bool{}
	for index, webhook := range config.Webhooks {
		field := fmt.Sprintf("notify.webhooks[%d]", index)
		parsed, err := url.Parse(webhook.URL)
		switch {
		case !checkNamePattern.MatchString(webhook.Name):
			return invalidConfig(field+".name", "must use only a-z, 0-9, _ and -")
		case names[webhook.Name]:
			return invalidConfig(field+".name", fmt.Sprintf("%q is used twice", webhook.Name))
		case err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "":
			return invalidConfig(field+".url", "must be an http or https URL")
		case webhook.Format != "" && !slices.Contains(notifyFormats, webhook.Format):
			return invalidConfig(field+".format", "must be json, slack, or discord")
		case webhook.On != "" && !slices.Contains(notifyLevels, webhook.On):
			return invalidConfig(field+".on", "must be fail, warn, or always")
		}
		names[webhook.Name] = true
	}
	return nil
}

// notifyWanted는 이 webhook이 이 결과에 보낼지다.
func notifyWanted(webhook notifyWebhook, summary Summary) bool {
	switch webhook.On {
	case "always":
		return true
	case "warn":
		return summary.Fail > 0 || summary.Warn > 0
	}
	return summary.Fail > 0
}

// notifyText는 사람이 읽을 메시지다. 첫 줄은 host와 명령, 판정 수이고 그 아래에 통과하지 못한 결과를 한 줄씩 적는다.
func notifyText(command string, report Report) string {
	host, _ := report.Host["hostname"].(string)
	head := "PASS"
	switch {
	case report.Summary.Fail > 0:
		head = "FAIL"
	case report.Summary.Warn > 0:
		head = "WARN"
	}
	lines := []string{fmt.Sprintf("[%s] %s edc %s: %d fail, %d warn, %d pass", head, host, command, report.Summary.Fail, report.Summary.Warn, report.Summary.Pass)}
	shown := 0
	for _, result := range report.Results {
		if result.Status != StatusFail && result.Status != StatusWarn {
			continue
		}
		if shown == notifyLines {
			lines = append(lines, "…")
			break
		}
		line := fmt.Sprintf("%s %s: %s", strings.ToUpper(string(result.Status)), result.Probe, infoSingleLine(result.Summary))
		if result.Next != "" {
			line += " · next: " + result.Next
		}
		lines = append(lines, svcTruncate(line, 300))
		shown++
	}
	return svcTruncate(strings.Join(lines, "\n"), notifyTextMax)
}

type notifyJSON struct {
	Host    string         `json:"host"`
	Command string         `json:"command"`
	Summary Summary        `json:"summary"`
	Results []notifyResult `json:"results"`
	Text    string         `json:"text"`
}

type notifyResult struct {
	Probe   string `json:"probe"`
	Status  Status `json:"status"`
	Summary string `json:"summary"`
	Next    string `json:"next,omitempty"`
}

// notifyBody는 형식에 맞는 요청 본문이다. json은 통과하지 못한 결과만 담는다. 전체 report는 --json으로 남긴다.
func notifyBody(format, command string, report Report) ([]byte, error) {
	text := notifyText(command, report)
	switch format {
	case "slack":
		return json.Marshal(map[string]string{"text": text})
	case "discord":
		return json.Marshal(map[string]string{"content": text})
	}
	host, _ := report.Host["hostname"].(string)
	body := notifyJSON{Host: host, Command: command, Summary: report.Summary, Text: text, Results: []notifyResult{}}
	for _, result := range report.Results {
		if result.Status == StatusFail || result.Status == StatusWarn {
			body.Results = append(body.Results, notifyResult{Probe: result.Probe, Status: result.Status, Summary: result.Summary, Next: result.Next})
		}
	}
	return json.Marshal(body)
}

// sendNotify는 기준에 맞는 webhook마다 POST한다. 실패한 webhook의 이유를 모아 돌려준다. URL은 비밀이라 오류에 적지 않는다.
func sendNotify(ctx context.Context, client *http.Client, webhooks []notifyWebhook, command, version string, report Report) []error {
	var errs []error
	for _, webhook := range webhooks {
		if !notifyWanted(webhook, report.Summary) {
			continue
		}
		body, err := notifyBody(webhook.Format, command, report)
		if err == nil {
			err = postNotify(ctx, client, webhook.URL, version, body)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", webhook.Name, err))
		}
	}
	return errs
}

func postNotify(ctx context.Context, client *http.Client, target, version string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return notifyHideURL(err, target)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "edc/"+version)
	response, err := client.Do(request)
	if err != nil {
		return notifyHideURL(err, target)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return nil
}

// notifyHideURL은 net/http 오류에 들어간 URL을 host만 남기고 지운다. Slack과 Discord는 경로 자체가 비밀 token이다.
func notifyHideURL(err error, target string) error {
	text := err.Error()
	if parsed, parseErr := url.Parse(target); parseErr == nil {
		text = strings.ReplaceAll(text, target, parsed.Scheme+"://"+parsed.Host+"/…")
	}
	return fmt.Errorf("%s", text)
}

// emitNotify는 결과를 출력한 뒤 --notify면 webhook으로 보낸다. 보내지 못하면 stderr에 적고, 결과가 통과였어도 종료 코드를
// 1로 만든다. 알림이 조용히 사라지면 감시가 멈춘 것을 모른다.
func emitNotify(options commonOptions, report Report, command, version string, notify bool) int {
	code := emit(options, report)
	if !notify {
		return code
	}
	if failed := notifyFailures(activeConfig, report, command, version); failed {
		return max(code, 1)
	}
	return code
}

func notifyFailures(config edcConfig, report Report, command, version string) bool {
	if len(config.Notify.Webhooks) == 0 {
		fmt.Fprintln(os.Stderr, T("notify.error.none"))
		return true
	}
	// 남이 고칠 수 있는 설정 파일의 URL로 보내면 결과가 다른 곳으로 샌다. check 루틴의 셸 명령과 같은 규칙을 쓴다.
	if config.commandSafety != nil {
		fmt.Fprintln(os.Stderr, T("notify.error.unsafe", config.commandSafety))
		return true
	}
	errs := sendNotify(context.Background(), &http.Client{}, config.Notify.Webhooks, command, version, report)
	for _, err := range errs {
		fmt.Fprintln(os.Stderr, T("notify.error.send", err))
	}
	return len(errs) > 0
}
