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
		"Content-Type":                "application/json",
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if m := req.Header.Get("X-Conversation-Message-ID"); len(m) != 32 || m != req.Header.Get("X-Request-ID") {
		t.Errorf("message id = %q / request id = %q", m, req.Header.Get("X-Request-ID"))
	}
	// 会话 / 轮级 ID 按账号派生（插件 own()）：32 hex，且不等于原值。
	for k, raw := range map[string]string{"X-Conversation-ID": "conv", "X-Conversation-Request-ID": "0123456789abcdef0123456789abcdef"} {
		if v := req.Header.Get(k); len(v) != 32 || v == raw {
			t.Errorf("%s = %q, want a per-account 32-hex id", k, v)
		}
	}
	for _, k := range []string{"Origin", "Referer", "X-CodeBuddy-Request", "X-Root-Request-ID", "Accept-Language", "X-Machine-ID",
		"X-Session-ID", "X-Trace-ID", "X-B3-TraceId", "X-B3-SpanId", "X-B3-Sampled", "X-No-Enterprise-Id",
		"X-No-Department-Info", "X-Forwarded-For", "X-Real-IP", "X-Client-IP", "X-Refresh-Token"} {
		if v := req.Header.Get(k); v != "" {
			t.Errorf("gateway-only header %s sent: %q", k, v)
		}
	}
}

// TestOfficialChatHeadersGlobal 国际版：workbuddy-ai 平台段、5.9.3（插件 SITES 表）、X-Domain www.workbuddy.ai、
// X-Agent-Runtime 两个版本都发。
func TestOfficialChatHeadersGlobal(t *testing.T) {
	c := New()
	a := &auth.Auth{AccessToken: "at", UID: "u2", Domain: "www.workbuddy.ai"}
	a.BackfillRealm()
	req, _ := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	c.ChatHeaders(req, a, "", ChatMeta{})
	if ua := req.Header.Get("User-Agent"); ua != "WorkBuddy/5.9.3 workbuddy-ai/5.9.3 CLI/2.165.0" {
		t.Errorf("UA = %q", ua)
	}
	if d := req.Header.Get("X-Domain"); d != "www.workbuddy.ai" {
		t.Errorf("X-Domain = %q", d)
	}
	if v := req.Header.Get("X-IDE-Version"); v != "5.9.3" {
		t.Errorf("X-IDE-Version = %q", v)
	}
	if v := req.Header.Get("X-Agent-Runtime"); v != "local-agent" {
		t.Errorf("X-Agent-Runtime = %q", v)
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

// TestOfficialIDsPerAccount 插件 own()：同一会话 / 轮次在不同账号下是不同 ID，同账号稳定；
// 调用方自己给的头原样发出；X-Request-ID 每条请求新生成。
func TestOfficialIDsPerAccount(t *testing.T) {
	c := New()
	meta := ChatMeta{ConversationID: "sess-1", ConversationRequestID: "turn-1"}
	send := func(a *auth.Auth, m ChatMeta) http.Header {
		req, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", nil)
		c.ChatHeaders(req, a, "", m)
		return req.Header
	}
	a1 := &auth.Auth{AccessToken: "x", UID: "u1"}
	a2 := &auth.Auth{AccessToken: "y", UID: "u2"}
	h1, h1b, h2 := send(a1, meta), send(a1, meta), send(a2, meta)
	for _, k := range []string{"X-Conversation-ID", "X-Conversation-Request-ID"} {
		if h1.Get(k) != h1b.Get(k) {
			t.Errorf("%s not stable for one account: %q vs %q", k, h1.Get(k), h1b.Get(k))
		}
		if h1.Get(k) == h2.Get(k) {
			t.Errorf("%s shared across accounts: %q", k, h1.Get(k))
		}
	}
	if h1.Get("X-Request-ID") == h1b.Get("X-Request-ID") {
		t.Error("X-Request-ID must be new per request")
	}
	own := send(a2, ChatMeta{ConversationID: "c", ConversationRequestID: "r", CallerConversationID: true, CallerRequestID: true})
	if own.Get("X-Conversation-ID") != "c" || own.Get("X-Conversation-Request-ID") != "r" {
		t.Errorf("caller ids not kept: %q %q", own.Get("X-Conversation-ID"), own.Get("X-Conversation-Request-ID"))
	}
}

// TestOfficialNonChatHeaders 插件的非 chat 调用：meter（billing）、token 刷新、config 探测。
func TestOfficialNonChatHeaders(t *testing.T) {
	c := New()
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", UID: "u", EnterpriseID: "e"}
	req, _ := http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/billing/meter/daily-checkin", nil)
	c.BillingHeaders(req, a)
	want := map[string]string{
		"Content-Type": "application/json", "Accept": "application/json", "User-Agent": "WorkBuddy/5.9.3",
		"Authorization": "Bearer at", "X-User-Id": "u", "X-Domain": "copilot.tencent.com", "X-Product": "SaaS",
		"X-IDE-Type": "WorkBuddy", "X-Enterprise-Id": "e", "X-Tenant-Id": "e",
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("meter %s = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"X-CodeBuddy-Request", "Accept-Language", "X-Requested-With", "Origin"} {
		if v := req.Header.Get(k); v != "" {
			t.Errorf("meter sent %s = %q", k, v)
		}
	}

	req, _ = http.NewRequest(http.MethodPost, "https://copilot.tencent.com/v2/plugin/auth/token/refresh", nil)
	c.RefreshHeaders(req, a)
	want = map[string]string{
		"Content-Type": "application/json", "Accept": "application/json", "User-Agent": "WorkBuddy/5.9.3",
		"X-Refresh-Token": "rt", "X-Auth-Refresh-Source": "plugin", "X-Domain": "copilot.tencent.com",
		"X-Enterprise-Id": "e", "X-Tenant-Id": "e",
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("refresh %s = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"Authorization", "X-CodeBuddy-Request", "Origin", "X-Machine-ID"} {
		if v := req.Header.Get(k); v != "" {
			t.Errorf("refresh sent %s = %q", k, v)
		}
	}

	req, _ = http.NewRequest(http.MethodGet, "https://copilot.tencent.com/console/enterprises/personal/models", nil)
	c.CommonHeaders(req, a)
	if req.Header.Get("User-Agent") != "WorkBuddy/5.9.3 WorkBuddy/5.9.3 CLI/2.165.0" || req.Header.Get("X-IDE-Name") != "WorkBuddy" ||
		req.Header.Get("Origin") != "" || req.Header.Get("X-CodeBuddy-Request") != "" {
		t.Errorf("config headers = %v", req.Header)
	}
}
