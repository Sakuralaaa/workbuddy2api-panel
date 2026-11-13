package upstream

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// 用例取自插件 filtered.test.mjs（itspig plugins #540 的真实帧）。

type cutReader struct {
	s   string
	cut int
}

func (r *cutReader) Read(p []byte) (int, error) {
	if r.s == "" {
		return 0, io.EOF
	}
	n := r.cut
	if n > len(r.s) {
		n = len(r.s)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.s[:n])
	r.s = r.s[n:]
	return n, nil
}

func through(t *testing.T, upstream string, cut int) string {
	t.Helper()
	out, err := io.ReadAll(stripFilterRefusal(io.NopCloser(&cutReader{s: upstream, cut: cut})))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func sseFrame(delta map[string]any, finish any) string {
	b, _ := json.Marshal(map[string]any{"id": "c1", "object": "chat.completion.chunk", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	return "data: " + string(b) + "\n\n"
}

// outChoices 解析输出各帧的 choices[0]。
func outChoices(t *testing.T, out string) []map[string]any {
	t.Helper()
	var cs []map[string]any
	for _, ev := range strings.Split(out, "\n\n") {
		if !strings.HasPrefix(ev, "data: {") {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ev, "data: ")), &v); err != nil {
			t.Fatalf("bad frame %q: %v", ev, err)
		}
		cs = append(cs, firstChoice(v))
	}
	return cs
}

func assertRefusalGone(t *testing.T, out, reasoning string) {
	t.Helper()
	cs := outChoices(t, out)
	var r strings.Builder
	for _, c := range cs {
		d, _ := c["delta"].(map[string]any)
		if s, _ := d["reasoning_content"].(string); s != "" {
			r.WriteString(s)
		}
		if s, _ := d["content"].(string); s != "" {
			t.Errorf("filter text still sent: %q", s)
		}
	}
	if r.String() != reasoning {
		t.Errorf("reasoning = %q, want %q", r.String(), reasoning)
	}
	if len(cs) == 0 || cs[len(cs)-1]["finish_reason"] != "content_filter" {
		t.Errorf("last finish_reason not content_filter: %v", cs)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("no [DONE]: %q", out)
	}
}

const filterLine = "Sorry, I can't respond to this question."

func TestRefusalRealFramesStripped(t *testing.T) {
	upstream := `data: {"choices":[{"delta":{"content":"","extra_fields":null,"function_call":null,"reasoning_content":" node","refusal":"","tool_calls":[]},"finish_reason":"","index":0,"logprobs":null}],"created":1793401068,"id":"c56c","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","usage":null}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"\nSorry, I can't respond to this question.\n","extra_fields":null,"function_call":null,"reasoning_content":"","refusal":"","role":"assistant","tool_calls":[]},"finish_reason":"content_filter","index":0,"logprobs":null}],"created":0,"id":"c56c","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","usage":null}` + "\n\n" +
		"data: [DONE]\n\n"
	for _, cut := range []int{1, 3, 7, 64, 1 << 20} {
		assertRefusalGone(t, through(t, upstream, cut), " node")
	}
}

func TestRefusalSplitOverDeltasStripped(t *testing.T) {
	d := func(s string) string { return sseFrame(map[string]any{"content": s, "tool_calls": []any{}}, "") }
	upstream := sseFrame(map[string]any{"role": "assistant", "content": "", "reasoning_content": "hm", "tool_calls": []any{}}, "") +
		d("\n") + d("Sorry") + d(", I can't respond") + d(" to this question.") + d("\n") +
		sseFrame(map[string]any{"content": ""}, "content_filter") + "data: [DONE]\n\n"
	assertRefusalGone(t, through(t, upstream, 5), "hm")
}

func TestRefusalLeavesAnswersAlone(t *testing.T) {
	cases := map[string]string{
		"same words ending stop": sseFrame(map[string]any{"reasoning_content": "hm"}, nil) + sseFrame(map[string]any{"content": "Sorry"}, nil) +
			sseFrame(map[string]any{"content": ", I can't respond to this question."}, nil) + sseFrame(map[string]any{}, "stop") + "data: [DONE]\n\n",
		"only begins like it": sseFrame(map[string]any{"content": "Sorry"}, nil) + sseFrame(map[string]any{"content": ", I can't find that file."}, nil) +
			sseFrame(map[string]any{}, "stop") + "data: [DONE]\n\n",
		"other words before filter": sseFrame(map[string]any{"content": "Here is the plan: "}, nil) + sseFrame(map[string]any{"content": filterLine}, nil) +
			sseFrame(map[string]any{}, "content_filter") + "data: [DONE]\n\n",
		"line followed by more words": sseFrame(map[string]any{"content": "\n" + filterLine + "\n"}, nil) + sseFrame(map[string]any{"content": "But here is the answer."}, nil) +
			sseFrame(map[string]any{}, "content_filter") + "data: [DONE]\n\n",
		"real tool call first": sseFrame(map[string]any{"content": "", "tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function", "function": map[string]any{"name": "grep", "arguments": "{}"}}}}, nil) +
			sseFrame(map[string]any{"content": filterLine}, nil) + sseFrame(map[string]any{}, "content_filter") + "data: [DONE]\n\n",
	}
	for name, upstream := range cases {
		for _, cut := range []int{2, 1 << 20} {
			if got := through(t, upstream, cut); got != upstream {
				t.Errorf("%s (cut %d) changed:\n got %q\nwant %q", name, cut, got, upstream)
			}
		}
	}
}

// 正文的第一个字不被扣住：读到第一帧正文时就已可读出。
func TestRefusalDoesNotHoldAnswers(t *testing.T) {
	pr, pw := io.Pipe()
	f := stripFilterRefusal(pr)
	go func() { _, _ = pw.Write([]byte(sseFrame(map[string]any{"content": "Hello"}, nil))) }()
	buf := make([]byte, 4096)
	n, err := f.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "Hello") {
		t.Fatalf("answer held: %q %v", buf[:n], err)
	}
	pw.Close()
}
