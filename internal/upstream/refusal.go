// refusal.go 去掉 WorkBuddy 安全过滤的固定拒答文本（对齐 magpie 官方 WorkBuddy 插件 unsaid）。
//
// WorkBuddy 的安全过滤拦下一条回复时，流里先给一句固定文本，再以 finish_reason
// "content_filter" 收尾：deepseek-v4.1-flash 在 copilot.tencent.com 上思考完、说一句
// "Sorry, I can't respond to this question."、content_filter 结束，输出 token 计 0
// （itspig，plugins #540），且常带前后换行（"\nSorry, I can't respond to this question.\n"）。
// 这句话本身不是模型的回答；留着它，客户端会把「200 + 一句话」当成模型的正常答复，
// 而不是一次被过滤的空回复。
//
// 只扣住「到目前为止仍可能是这句过滤文本」的正文帧：任何其他回复的第一个字照常即时
// 下发；开头恰好像它的回复，在出现第一个不像的字或流结束时原样放行。只有在
// content_filter 收尾时，扣住的帧才去掉 content 后发出（只剩 content 的帧整帧丢弃）。
package upstream

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// filterSays 过滤器的固定拒答文本（插件 FILTER_SAYS）。
var filterSays = []string{"Sorry, I can't respond to this question."}

// filterSaid 插件同名函数：到目前为止的正文是否仍可能是过滤文本（两侧可带空白；
// 弯引号按直引号算）。只有空白时也算「可能」。
func filterSaid(text string) bool {
	t := strings.TrimSpace(strings.ReplaceAll(text, "’", "'"))
	for _, s := range filterSays {
		if strings.HasPrefix(s, t) {
			return true
		}
	}
	return false
}

var sseDataLine = regexp.MustCompile(`(?m)^data:\s?(.*)$`)

// sseChunk 取事件里第一行 data: 的 JSON 对象；[DONE] / 非 JSON 返回 nil。
func sseChunk(ev string) map[string]any {
	m := sseDataLine.FindStringSubmatch(ev)
	if m == nil || m[1] == "[DONE]" {
		return nil
	}
	var v map[string]any
	if json.Unmarshal([]byte(m[1]), &v) != nil {
		return nil
	}
	return v
}

func firstChoice(v map[string]any) map[string]any {
	chs, _ := v["choices"].([]any)
	if len(chs) == 0 {
		return nil
	}
	c, _ := chs[0].(map[string]any)
	return c
}

// refusalFilter 包装上游 SSE 流（插件 unsaid 的 TransformStream）。
type refusalFilter struct {
	src  io.ReadCloser
	rest string   // 尚未凑成完整事件的输入
	held []string // 可能是过滤文本时扣住的事件
	said string   // 到目前为止的正文
	open bool     // 回复仍可能是过滤文本
	out  bytes.Buffer
	done bool
	err  error
	buf  []byte
}

// stripFilterRefusal 返回去掉过滤拒答文本的 SSE 流；Close 关闭底层流。
func stripFilterRefusal(src io.ReadCloser) io.ReadCloser {
	return &refusalFilter{src: src, open: true, buf: make([]byte, 32*1024)}
}

func (f *refusalFilter) Close() error { return f.src.Close() }

func (f *refusalFilter) Read(p []byte) (int, error) {
	for f.out.Len() == 0 {
		if f.done {
			return 0, f.err
		}
		if !f.open && f.rest == "" && len(f.held) == 0 {
			// 判定已结束：之后的字节原样直通。
			return f.src.Read(p)
		}
		n, err := f.src.Read(f.buf)
		if n > 0 {
			f.feed(string(f.buf[:n]))
		}
		if err != nil {
			f.flushAll()
			f.done = true
			f.err = err
		}
	}
	return f.out.Read(p)
}

// feed 插件 transform：拼接、归一 \r\n、按空行切事件、逐个判定。
func (f *refusalFilter) feed(chunk string) {
	if !f.open && len(f.held) == 0 {
		f.out.WriteString(f.rest + chunk)
		f.rest = ""
		return
	}
	f.rest = strings.ReplaceAll(f.rest+chunk, "\r\n", "\n")
	evs := strings.Split(f.rest, "\n\n")
	f.rest = evs[len(evs)-1]
	for _, ev := range evs[:len(evs)-1] {
		for _, e := range f.event(ev) {
			f.out.WriteString(e + "\n\n")
		}
	}
	if !f.open && len(f.held) == 0 && f.rest != "" {
		f.out.WriteString(f.rest)
		f.rest = ""
	}
}

// flushAll 插件 flush：流结束时扣住的事件与残余原样放行。
func (f *refusalFilter) flushAll() {
	for _, e := range f.held {
		f.out.WriteString(e + "\n\n")
	}
	f.held = nil
	f.out.WriteString(f.rest)
	f.rest = ""
}

func (f *refusalFilter) release(ev string) []string {
	f.open = false
	out := append(f.held, ev)
	f.held = nil
	return out
}

// event 插件 unsaid.event。
func (f *refusalFilter) event(ev string) []string {
	if !f.open {
		return []string{ev}
	}
	c := firstChoice(sseChunk(ev))
	var delta map[string]any
	if c != nil {
		delta, _ = c["delta"].(map[string]any)
	}
	text, _ := delta["content"].(string)
	finish, _ := c["finish_reason"].(string)
	// 上游每个 delta 都带 "tool_calls": []：只有非空列表才算调用。
	calling := false
	if calls, present := delta["tool_calls"]; present && calls != nil {
		if list, ok := calls.([]any); ok {
			calling = len(list) > 0
		} else {
			calling = true
		}
	}
	if calling || (text != "" && !filterSaid(f.said+text)) {
		return f.release(ev)
	}
	f.said += text
	if finish == "content_filter" && f.said != "" {
		all := f.release(ev)
		out := all[:0]
		for _, e := range all {
			if w := withoutContent(e); w != "" {
				out = append(out, w)
			}
		}
		return out
	}
	if finish != "" {
		return f.release(ev)
	}
	if f.said == "" {
		return []string{ev} // 思考、role 帧：在任何正文之前照常下发
	}
	f.held = append(f.held, ev)
	return nil
}

// withoutContent 插件 without：去掉事件的 delta.content；除 role 外什么都不剩、且无
// finish_reason / usage 的帧返回 ""（整帧丢弃）。
func withoutContent(ev string) string {
	v := sseChunk(ev)
	c := firstChoice(v)
	if c == nil {
		return ev
	}
	delta, ok := c["delta"].(map[string]any)
	if !ok {
		return ev
	}
	if _, ok := delta["content"].(string); !ok {
		return ev
	}
	delete(delta, "content")
	left := 0
	for k := range delta {
		if k != "role" {
			left++
		}
	}
	if left == 0 && c["finish_reason"] == nil && v["usage"] == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ev
	}
	return "data: " + string(raw)
}
