package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func chatReq(key, model string) *http.Request {
	r := httptest.NewRequest("POST", "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	return r
}

// 多 API Key：config api_key 与密钥表里启用的 key 都能调用；停用 / 删除 / 错误的 key 401；
// 每次请求的用量按 key 计入（请求数、成功失败、tokens），请求日志记录所用 key。
func TestMultiKeyAuthAndUsage(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	keys, _ := apikeys.Open("")
	k1, s1, _ := keys.Create("客户端1")
	_, s2, _ := keys.Create("客户端2")
	rl := reqlog.New(reqlog.Config{})
	defer rl.Close()
	h := NewHandler(Config{
		Pool:       testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream:   up,
		APIKey:     "legacy-key",
		Keys:       keys,
		RequestLog: rl,
	})
	for _, tc := range []struct {
		key  string
		want int
	}{{"legacy-key", 200}, {s1, 200}, {s2, 200}, {"bogus", 401}, {"", 401}} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, chatReq(tc.key, "glm-5.2"))
		if rec.Code != tc.want {
			t.Fatalf("key %q: code=%d want %d body=%s", tc.key, rec.Code, tc.want, rec.Body)
		}
	}
	_ = keys.SetEnabled(k1.ID, false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq(s1, "glm-5.2"))
	if rec.Code != 401 {
		t.Fatalf("disabled key code=%d", rec.Code)
	}
	rep := keys.Report("", "")
	by := map[string]apikeys.KeyReport{}
	for _, k := range rep.Keys {
		by[k.ID] = k
	}
	if by[k1.ID].Total.Requests != 1 || by[k1.ID].Total.Success != 1 || by[k1.ID].Total.TotalTokens != 2 {
		t.Fatalf("k1 usage=%+v", by[k1.ID].Total)
	}
	if by[apikeys.LegacyID].Total.Requests != 1 {
		t.Fatalf("legacy usage=%+v", by[apikeys.LegacyID].Total)
	}
	if m := by[k1.ID].ByModel["glm-5.2"]; m.Requests != 1 {
		t.Fatalf("by model=%+v", by[k1.ID].ByModel)
	}
	// 401 的请求不计入任何 key
	if rep.Total.Requests != 3 {
		t.Fatalf("total=%+v", rep.Total)
	}
	var ids []string
	for _, e := range rl.Snapshot().Recent {
		ids = append(ids, e.APIKeyID)
	}
	if !strings.Contains(strings.Join(ids, ","), k1.ID) || !strings.Contains(strings.Join(ids, ","), apikeys.LegacyID) {
		t.Fatalf("request log key ids=%v", ids)
	}
}

// 只有密钥表（config api_key 为空）时同样强制鉴权；两者都空 = 不鉴权（旧行为）。
func TestKeysOnlyAuth(t *testing.T) {
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true })
	pool := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	keys, _ := apikeys.Open("")
	h := NewHandler(Config{Pool: pool, Upstream: up, Keys: keys})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("", "glm-5.2"))
	if rec.Code != 200 {
		t.Fatalf("no keys at all: code=%d", rec.Code)
	}
	_, sec, _ := keys.Create("only")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("", "glm-5.2"))
	if rec.Code != 401 {
		t.Fatalf("keys-only without bearer: code=%d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq(sec, "glm-5.2"))
	if rec.Code != 200 {
		t.Fatalf("keys-only with key: code=%d", rec.Code)
	}
	// 进程内面板请求免鉴权
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("", "glm-5.2").WithContext(WithInternal(context.Background())))
	if rec.Code != 200 {
		t.Fatalf("internal request: code=%d", rec.Code)
	}
}

// 模型停用：/v1/models 不列出，点名调用 403 model_disabled 且不打上游；面板内部测试放行；
// 经 livecfg 热改立即生效。
func TestModelDisabledToggle(t *testing.T) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.2"}, {ID: "kimi-k2"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()
	defer func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = nil
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	}()
	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) { calls++; return 200, sseOK, true })
	live := livecfg.New(livecfg.Snapshot{DisabledModels: livecfg.NormalizeHiddenModels([]string{"cn:kimi-k2"})})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up, Live: live})

	list := func() map[string]bool {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		var resp struct {
			Data []struct{ ID string } `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		out := map[string]bool{}
		for _, m := range resp.Data {
			out[m.ID] = true
		}
		return out
	}
	if ids := list(); ids["cn:kimi-k2"] || !ids["cn:glm-5.2"] {
		t.Fatalf("models list=%v", ids)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("", "cn:kimi-k2"))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"model_disabled"`) {
		t.Fatalf("disabled call code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 0 {
		t.Fatalf("disabled model reached upstream")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("", "kimi-k2").WithContext(WithInternal(context.Background())))
	if rec.Code != 200 {
		t.Fatalf("internal test of disabled model code=%d body=%s", rec.Code, rec.Body)
	}
	// 重新启用（热改）
	live.Store(livecfg.Snapshot{})
	if ids := list(); !ids["cn:kimi-k2"] {
		t.Fatalf("re-enabled model missing: %v", ids)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, chatReq("", "cn:kimi-k2"))
	if rec.Code != 200 {
		t.Fatalf("re-enabled call code=%d", rec.Code)
	}
}
