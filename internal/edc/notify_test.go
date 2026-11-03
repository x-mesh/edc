package edc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testNotifyReport() Report {
	results := []Result{
		{Probe: "check.disk", Status: StatusFail, Summary: "/ 97%", Next: "edc du /"},
		{Probe: "check.cert", Status: StatusWarn, Summary: "/etc/a.crt expires in 10 days"},
		{Probe: "check.memory", Status: StatusPass, Summary: "ok"},
	}
	return Report{Host: map[string]interface{}{"hostname": "web-1"}, Results: results, Summary: summarize(results)}
}

func TestNotifyConfigFromTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	text := "[[notify.webhooks]]\nname = \"ops\"\nurl = \"https://hooks.slack.com/services/T/B/x\"\nformat = \"slack\"\non = \"warn\"\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadConfigAt(path)
	if err != nil || len(config.Notify.Webhooks) != 1 || config.Notify.Webhooks[0].On != "warn" {
		t.Fatalf("config = %+v, %v", config.Notify, err)
	}
	for name, webhook := range map[string]notifyWebhook{
		"name":   {Name: "Ops!", URL: "https://x"},
		"scheme": {Name: "ops", URL: "file:///etc/passwd"},
		"host":   {Name: "ops", URL: "https://"},
		"format": {Name: "ops", URL: "https://x", Format: "teams"},
		"on":     {Name: "ops", URL: "https://x", On: "pass"},
	} {
		if err := validateNotifyConfig(notifyConfig{Webhooks: []notifyWebhook{webhook}}); err == nil {
			t.Errorf("%s: %+v must be rejected", name, webhook)
		}
	}
	twice := notifyConfig{Webhooks: []notifyWebhook{{Name: "ops", URL: "https://a"}, {Name: "ops", URL: "https://b"}}}
	if err := validateNotifyConfig(twice); err == nil {
		t.Fatal("a name used twice must be rejected")
	}
}

func TestNotifyWantedFollowsTheLevel(t *testing.T) {
	warnOnly := Summary{Warn: 1}
	for _, test := range []struct {
		on      string
		summary Summary
		want    bool
	}{
		{"", Summary{Fail: 1}, true}, {"", warnOnly, false}, {"fail", warnOnly, false},
		{"warn", warnOnly, true}, {"warn", Summary{Pass: 3}, false}, {"always", Summary{Pass: 3}, true},
	} {
		if got := notifyWanted(notifyWebhook{On: test.on}, test.summary); got != test.want {
			t.Errorf("on=%q %+v = %v, want %v", test.on, test.summary, got, test.want)
		}
	}
}

func TestNotifyBodiesCarryTheFindings(t *testing.T) {
	report := testNotifyReport()
	text := notifyText("check", report)
	lines := strings.Split(text, "\n")
	if lines[0] != "[FAIL] web-1 edc check: 1 fail, 1 warn, 1 pass" || lines[1] != "FAIL check.disk: / 97% · next: edc du /" || len(lines) != 3 {
		t.Fatalf("text = %q", text)
	}
	for format, key := range map[string]string{"slack": "text", "discord": "content"} {
		body, _ := notifyBody(format, "check", report)
		var decoded map[string]string
		if err := json.Unmarshal(body, &decoded); err != nil || decoded[key] != text {
			t.Errorf("%s = %s", format, body)
		}
	}
	body, _ := notifyBody("", "check", report)
	var decoded notifyJSON
	if err := json.Unmarshal(body, &decoded); err != nil || decoded.Host != "web-1" || len(decoded.Results) != 2 || decoded.Summary.Fail != 1 {
		t.Fatalf("json = %s", body)
	}
	many := Report{Host: report.Host}
	for range 15 {
		many.Results = append(many.Results, Result{Probe: "check.x", Status: StatusWarn, Summary: strings.Repeat("y", 400)})
	}
	many.Summary = summarize(many.Results)
	if text := notifyText("check", many); len([]rune(text)) > notifyTextMax+1 || strings.Count(text, "\n") > notifyLines+1 {
		t.Fatalf("long text = %d runes, %d lines", len([]rune(text)), strings.Count(text, "\n"))
	}
}

func TestSendNotifyPostsAndHidesTheURL(t *testing.T) {
	var received []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		received = append(received, request.URL.Path+" "+request.Header.Get("Content-Type")+" "+request.Header.Get("User-Agent")+" "+string(body))
		if strings.Contains(request.URL.Path, "broken") {
			writer.WriteHeader(http.StatusForbidden)
		}
	}))
	defer server.Close()
	webhooks := []notifyWebhook{
		{Name: "ops", URL: server.URL + "/hook", Format: "slack", On: "warn"},
		{Name: "quiet", URL: server.URL + "/quiet", On: "fail"},
		{Name: "broken", URL: server.URL + "/broken/secret-token"},
	}
	report := testNotifyReport()
	report.Results, report.Summary = report.Results[1:], summarize(report.Results[1:])
	errs := sendNotify(context.Background(), server.Client(), webhooks, "check", "1.2.3", report)
	// fail이 없으므로 기본 기준(fail)인 webhook은 보내지 않는다.
	if len(received) != 1 || !strings.HasPrefix(received[0], "/hook application/json edc/1.2.3 {\"text\":\"[WARN] web-1") || len(errs) != 0 {
		t.Fatalf("received = %v, errs = %v", received, errs)
	}
	report = testNotifyReport()
	errs = sendNotify(context.Background(), server.Client(), webhooks, "check", "1.2.3", report)
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "broken: HTTP 403") {
		t.Fatalf("errs = %v", errs)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	errs = sendNotify(context.Background(), &http.Client{}, []notifyWebhook{{Name: "gone", URL: closed.URL + "/services/T/B/secret"}}, "check", "1", report)
	if len(errs) != 1 || strings.Contains(errs[0].Error(), "secret") {
		t.Fatalf("the URL path must stay out of the error: %v", errs)
	}
}

func TestNotifyFailuresRefusesAnUnsafeConfig(t *testing.T) {
	report := testNotifyReport()
	if !notifyFailures(edcConfig{}, report, "check", "1") {
		t.Fatal("--notify without a webhook must fail")
	}
	unsafe := edcConfig{Notify: notifyConfig{Webhooks: []notifyWebhook{{Name: "ops", URL: "http://127.0.0.1:1/x"}}}, commandSafety: errors.New("config.toml is writable by others")}
	if !notifyFailures(unsafe, report, "check", "1") {
		t.Fatal("a config that others can change must not send")
	}
}
