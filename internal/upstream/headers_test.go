package upstream

import (
	"net/http"
	"regexp"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestChatHeadersAlwaysSendConversationTrio 回归 issue #190：国际端点收紧通道
// 校验后，缺 X-Conversation-ID 的聊天请求会 400 / 11128（conversation id
// missing）。即使调用方没给会话 ID（零值 ChatMeta），三元组也必须齐全，
// 且兜底 ID 是 32 位 hex。
func TestChatHeadersAlwaysSendConversationTrio(t *testing.T) {
	c := &Client{}
	req, err := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.ChatHeaders(req, testAuth(), "", ChatMeta{})

	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	for _, h := range []string{
		"X-Conversation-ID",
		"X-Conversation-Request-ID",
		"X-Conversation-Message-ID",
	} {
		v := req.Header.Get(h)
		if v == "" {
			t.Fatalf("%s missing (通道校验会拒绝缺少会话三元组的请求)", h)
		}
		if !hex32.MatchString(v) {
			t.Errorf("%s = %q, want 32 hex", h, v)
		}
	}

	// 兜底会话 ID 每次请求新生成（不是常量），且与 request/message ID 不冲突。
	req2, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req2, testAuth(), "", ChatMeta{})
	if req.Header.Get("X-Conversation-ID") == req2.Header.Get("X-Conversation-ID") {
		t.Error("fallback X-Conversation-ID should be fresh per request")
	}
}

// TestChatHeadersPreservesConversationID 调用方在请求头里给的会话 ID 原样透传，不被兜底覆盖；
// 不是调用方头的会话 ID（body / 会话键）按账号派生，稳定且不是原值。
func TestChatHeadersPreservesConversationID(t *testing.T) {
	c := &Client{}
	const conv = "0123456789abcdef0123456789abcdef"
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req, testAuth(), "", ChatMeta{ConversationID: conv, CallerConversationID: true})
	if got := req.Header.Get("X-Conversation-ID"); got != conv {
		t.Errorf("X-Conversation-ID = %q, want supplied %q", got, conv)
	}
	req2, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req2, testAuth(), "", ChatMeta{ConversationID: conv})
	req3, _ := http.NewRequest(http.MethodPost, "https://example.com/v2/chat/completions", nil)
	c.ChatHeaders(req3, testAuth(), "", ChatMeta{ConversationID: conv})
	got := req2.Header.Get("X-Conversation-ID")
	if got == conv || got != req3.Header.Get("X-Conversation-ID") {
		t.Errorf("derived X-Conversation-ID = %q / %q, want a stable per-account id", got, req3.Header.Get("X-Conversation-ID"))
	}
}

// testAuth 返回最小的 CN 账号（ChatHeaders 会读 UID/凭证，nil 会解引用）。
func testAuth() *auth.Auth {
	return &auth.Auth{UID: "u1", AccessToken: "at"}
}
