package upstream

import (
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestOfficialChatHeadersShape 默认（official）chat 头族与官方桌面端 CLI 实抓对齐：
// 必发的头齐全，网关特有的头一个不发。
func TestOfficialChatHeadersShape(t *testing.T) {
	c := New()
	c.SDKOS, c.SDKArch = "MacOS", "arm64"
	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1", Domain: "copilot.tencent.com"}
	req, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "1.2.3.4", ChatMeta{ConversationID: "conv", ConversationRequestID: "0123456789abcdef0123456789abcdef"})
	want := map[string]string{
		"Authorization":               "Bearer at",
		"X-User-Id":                   "u1",
		"X-Domain":                    "copilot.tencent.com",
		"X-Enterprise-Id":             "e1",
		"X-Tenant-Id":                 "e1",
		"X-Product":                   "SaaS",
		"X-IDE-Type":                  "WorkBuddy",
		"X-IDE-Name":                  "WorkBuddy",
		"X-IDE-Version":               "5.9.3",
		"User-Agent":                  "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0",
		"X-Requested-With":            "XMLHttpRequest",
		"X-Agent-Intent":              "craft",
		"X-Agent-Purpose":             "conversation",
		"X-Agent-Type":                "main",
		"X-Agent-Runtime":             "local-agent",
		"Accept":                      "application/json",
		"X-Stainless-Lang":            "js",
		"X-Stainless-Package-Version": "6.25.0",
		"X-Stainless-OS":              "MacOS",
		"X-Stainless-Arch":            "arm64",
		"X-Stainless-Runtime":         "node",
		"X-Stainless-Runtime-Version": "v22.21.1",
		"X-Stainless-Retry-Count":     "0",
		"X-Stainless-Timeout":         "1200",
		"X-Conversation-ID":           "conv",
		"X-Conversation-Request-ID":   "0123456789abcdef0123456789abcdef",
		"X-Root-Request-ID":           "0123456789abcdef0123456789abcdef",
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if m := req.Header.Get("X-Conversation-Message-ID"); len(m) != 32 || m != req.Header.Get("X-Request-ID") {
		t.Errorf("message id = %q / request id = %q", m, req.Header.Get("X-Request-ID"))
	}
	for _, k := range []string{"Origin", "Referer", "X-CodeBuddy-Request", "Accept-Language", "X-Machine-ID",
		"X-Session-ID", "X-Trace-ID", "X-B3-TraceId", "X-B3-SpanId", "X-B3-Sampled", "X-No-Enterprise-Id",
		"X-No-Department-Info", "X-Forwarded-For", "X-Real-IP", "X-Client-IP", "X-Refresh-Token"} {
		if v := req.Header.Get(k); v != "" {
			t.Errorf("gateway-only header %s sent: %q", k, v)
		}
	}
}

// TestOfficialChatHeadersGlobal 国际版：workbuddy-ai 平台段、5.9.4、X-Domain www.workbuddy.ai。
func TestOfficialChatHeadersGlobal(t *testing.T) {
	c := New()
	a := &auth.Auth{AccessToken: "at", UID: "u2", Domain: "www.workbuddy.ai"}
	a.BackfillRealm()
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})
	if ua := req.Header.Get("User-Agent"); ua != "WorkBuddy/5.9.4 workbuddy-ai/5.9.4 CLI/2.165.0" {
		t.Errorf("UA = %q", ua)
	}
	if d := req.Header.Get("X-Domain"); d != "www.workbuddy.ai" {
		t.Errorf("X-Domain = %q", d)
	}
	if v := req.Header.Get("X-IDE-Version"); v != "5.9.4" {
		t.Errorf("X-IDE-Version = %q", v)
	}
	if req.Header.Get("X-Conversation-ID") == "" {
		t.Error("X-Conversation-ID missing (intl answers 11128 without it)")
	}
}

// TestLegacyHeaderProfile header_profile=legacy 回退旧头族。
func TestLegacyHeaderProfile(t *testing.T) {
	c := New()
	c.HeaderProfile = "Legacy"
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	req, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})
	if req.Header.Get("X-CodeBuddy-Request") != "1" || !strings.HasPrefix(req.Header.Get("X-B3-TraceId"), "") || req.Header.Get("Origin") == "" {
		t.Errorf("legacy profile lost legacy headers: %v", req.Header)
	}
}
