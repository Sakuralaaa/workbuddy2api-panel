package upstream

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// plugin_align_test.go 钉住与 magpie 官方 WorkBuddy 插件（0.2.34）对齐的请求体整形：
// 默认档位、小预算不补档、reasoning 回传字段、默认 system、gzip。

func prepObj(t *testing.T, body string, efforts map[string][]string, defs map[string]string) map[string]any {
	t.Helper()
	out := PrepareBodyOptWithEffortsAndDefault([]byte(body), false, efforts, defs)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("out not json: %v (%s)", err, out)
	}
	return obj
}

// 插件 default-effort.test.mjs：没点名档位时补模型配置的默认档；点名的（含 none）原样；
// max_tokens ≤ 1024 不补；默认档不在档位表里不补。
func TestDefaultEffortFromConfig(t *testing.T) {
	efforts := map[string][]string{"glm-5.3": {"low", "high", "max"}, "kimi-k3": {"low", "high"}}
	defs := map[string]string{"glm-5.3": "high", "kimi-k3": "max", "deepseek-v4.1-flash": "high"}
	cases := []struct {
		name, body string
		want       any // nil = 不应有 reasoning_effort
	}{
		{"default applied", `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"named kept", `{"model":"glm-5.3","reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`, "low"},
		{"none kept", `{"model":"glm-5.3","reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}`, "none"},
		{"reasoning.effort counts as named", `{"model":"glm-5.3","reasoning":{"effort":"low"},"messages":[{"role":"user","content":"hi"}]}`, nil},
		{"small budget", `{"model":"glm-5.3","max_tokens":64,"messages":[{"role":"user","content":"title"}]}`, nil},
		{"small budget alias", `{"model":"glm-5.3","max_completion_tokens":1024,"messages":[{"role":"user","content":"title"}]}`, nil},
		{"budget above limit", `{"model":"glm-5.3","max_tokens":1025,"messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"default outside levels", `{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}`, nil},
		{"unknown model", `{"model":"other","messages":[{"role":"user","content":"hi"}]}`, nil},
		{"deepseek small budget: no effort, no thinking", `{"model":"deepseek-v4.1-flash","max_tokens":64,"messages":[{"role":"user","content":"t"}]}`, nil},
		{"deepseek default", `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"t"}]}`, "high"},
	}
	for _, c := range cases {
		obj := prepObj(t, c.body, efforts, defs)
		got, has := obj["reasoning_effort"]
		if c.want == nil {
			if has {
				t.Errorf("%s: reasoning_effort = %v, want none", c.name, got)
			}
		} else if got != c.want {
			t.Errorf("%s: reasoning_effort = %v, want %v", c.name, got, c.want)
		}
		if strings.HasPrefix(c.name, "deepseek small") {
			if _, has := obj["thinking"]; has {
				t.Errorf("%s: thinking injected on a small budget", c.name)
			}
		}
	}
}

// 插件 reasoning.test.mjs：assistant 的 reasoning_content / reasoning_details 以 reasoning 回传。
func TestReasoningSentAsReasoning(t *testing.T) {
	obj := prepObj(t, `{"model":"kimi-k2.6","messages":[
		{"role":"user","content":"u"},
		{"role":"assistant","content":"a","reasoning_content":"codeword"},
		{"role":"assistant","content":"b","reasoning_details":[{"type":"reasoning.text","text":"det"},{"type":"reasoning.summary","summary":"ails"}]},
		{"role":"assistant","content":"c","reasoning":"own","reasoning_content":"other"},
		{"role":"user","content":"u2"}]}`, nil, nil)
	msgs := obj["messages"].([]any)
	want := []string{"", "codeword", "details", "own", ""}
	// PrepareBody 不补 system（ChatStream 才补），messages 与输入一一对应。
	for i, w := range want {
		m := msgs[i].(map[string]any)
		if _, has := m["reasoning_content"]; has {
			t.Errorf("msg %d still has reasoning_content", i)
		}
		if _, has := m["reasoning_details"]; has {
			t.Errorf("msg %d still has reasoning_details", i)
		}
		if r, _ := m["reasoning"].(string); r != w {
			t.Errorf("msg %d reasoning = %q, want %q", i, r, w)
		}
	}
}

// 插件 withSystem（两个版本）+ gzipped（chat completions）：CN 首条非 system 也补默认 system；
// official 形态 chat 体 gzip + Content-Encoding；legacy 明文。
func TestChatStreamDefaultSystemAndGzip(t *testing.T) {
	for _, profile := range []string{"", HeaderProfileLegacy} {
		var enc string
		var body []byte
		c := testClient(func(r *http.Request) (*http.Response, error) {
			enc = r.Header.Get("Content-Encoding")
			body, _ = readTestBody(r)
			return jsonResp(200, "data: [DONE]\n\n"), nil
		})
		c.HeaderProfile = profile
		rc, _, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u"}, []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{})
		if err != nil {
			t.Fatal(err)
		}
		rc.Close()
		wantEnc := "gzip"
		if profile == HeaderProfileLegacy {
			wantEnc = ""
		}
		if enc != wantEnc {
			t.Errorf("profile %q: Content-Encoding = %q, want %q", profile, enc, wantEnc)
		}
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("profile %q: body not json: %v", profile, err)
		}
		first := obj["messages"].([]any)[0].(map[string]any)
		if first["role"] != "system" || first["content"] != "You are a helpful assistant." {
			t.Errorf("profile %q: first message = %v", profile, first)
		}
	}
}
