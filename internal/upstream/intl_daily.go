// intl_daily.go 国际版（WorkBuddy AI）每日活跃 + Buddy 盲盒（移植自 WorkBuddy-plugins
// v0.3.5 internal/jobs/intl.go 与 daily.go openBoxes）。
//
//   - 每日活跃：桌面通道一次短对话（走网关既有 ChatStream，与真实调用同一套签名/头）；
//     可选网页通道：在 workbuddy.ai 控制台建一条云端会话，经 ACP（SSE + JSON-RPC POST）
//     跑完一轮（计入官方每日活跃积分的那一条），结束后停止会话释放名额。
//   - 盲盒：旅行结束后用多余能量开 Buddy 盲盒（/v2/activity/growth/buddy/quota → open）。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// IntlWebUA 网页通道 UA（与插件 webUA 一致：桌面 Chrome）。
const IntlWebUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

// IntlWebTurnWait 网页会话最长等待（测试可缩短）。
var IntlWebTurnWait = 120 * time.Second

// intlWebPoll 会话状态轮询间隔（测试可缩短）。
var intlWebPoll = 3 * time.Second

// QuickChat 发一次极短的真实对话（流式读干），返回首个错误。供每日活跃 / 模型测试类任务复用。
func (c *Client) QuickChat(ctx context.Context, a *auth.Auth, model, prompt string) error {
	if prompt == "" {
		prompt = "Hi"
	}
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]any{{"role": "user", "content": prompt}},
		"stream":   true,
	})
	rc, status, respBody, err := c.ChatStreamContext(ctx, a, body, "", ChatMeta{})
	if err != nil || status >= 400 {
		if rc != nil {
			rc.Close()
		}
		if err == nil {
			err = fmt.Errorf("http %d: %.160s", status, respBody)
		}
		return err
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	return err
}

// IntlDailyResult 每日活跃结果。
type IntlDailyResult struct {
	Model     string `json:"model"`
	Web       bool   `json:"web"`
	WebStatus string `json:"web_status,omitempty"`
	Chunks    int64  `json:"chunks,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms,omitempty"`
}

// RunIntlDaily 国际版账号的每日活跃：桌面通道依次尝试 models 直到一个成功；web=true 再跑网页通道。
func (c *Client) RunIntlDaily(ctx context.Context, a *auth.Auth, models []string, web bool, prompt string) (*IntlDailyResult, error) {
	if a == nil || !a.IsGlobal() {
		return nil, errors.New("每日活跃只适用于国际版账号")
	}
	if len(models) == 0 {
		models = []string{"deepseek-v4.1-flash"}
	}
	if prompt == "" {
		prompt = "Hi"
	}
	res := &IntlDailyResult{}
	var lastErr error
	for _, m := range models {
		if err := c.QuickChat(ctx, a, m, prompt); err != nil {
			lastErr = err
			if strings.Contains(err.Error(), "11140") {
				break
			}
			continue
		}
		res.Model = m
		break
	}
	if res.Model == "" {
		return res, fmt.Errorf("桌面通道对话失败: %v", lastErr)
	}
	if !web {
		return res, nil
	}
	res.Web = true
	wr, err := c.IntlWebSession(ctx, a, prompt, res.Model)
	if wr != nil {
		res.WebStatus, res.Chunks, res.ElapsedMS = wr.Status, wr.Chunks, wr.ElapsedMS
	}
	if err != nil {
		return res, fmt.Errorf("网页通道失败: %w", err)
	}
	return res, nil
}

// IntlWebResult 网页会话结果。
type IntlWebResult struct {
	Conversation string
	Status       string
	Chunks       int64
	Events       int64
	ElapsedMS    int64
}

func (c *Client) intlConsoleBase(a *auth.Auth) string {
	return strings.TrimRight(c.chatBase(a), "/") + "/console/as/conversations/"
}

func (c *Client) intlWebReq(ctx context.Context, a *auth.Auth, method, u string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(c.chatBase(a), "/")
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("X-User-Id", a.UID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/app")
	req.Header.Set("User-Agent", IntlWebUA)
	return c.doJSON(req)
}

// IntlWebSession 建网页会话 → 读沙箱 → ACP 跑一轮 → 停止（失败的会话顺带删除）。
func (c *Client) IntlWebSession(ctx context.Context, a *auth.Auth, prompt, model string) (*IntlWebResult, error) {
	body := map[string]any{
		"prompt": prompt, "model": model, "conversationOrigin": "workbuddy-app",
		"plugins": []map[string]string{{"name": "weixinpay", "marketplace": "codebuddy-builtin"}},
	}
	base := c.intlConsoleBase(a)
	create := func() (string, error) {
		data, err := c.intlWebReq(ctx, a, http.MethodPost, base, body)
		if err != nil {
			return "", err
		}
		var d struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(data, &d)
		if d.ID == "" {
			return "", errors.New("建会话失败：上游未返回会话 id")
		}
		return d.ID, nil
	}
	conv, err := create()
	if err != nil && (strings.Contains(err.Error(), "10105") || strings.Contains(err.Error(), "http 429") || strings.Contains(strings.ToLower(err.Error()), "limit")) {
		if c.intlWebPrune(ctx, a, prompt) > 0 {
			conv, err = create()
		}
	}
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		c.intlWebAction(cctx, a, conv, "stop")
		if !keep {
			c.intlWebAction(cctx, a, conv, "delete")
		}
	}()
	data, err := c.intlWebReq(ctx, a, http.MethodGet, base+url.PathEscape(conv)+"/session", nil)
	if err != nil {
		return nil, fmt.Errorf("沙箱查询失败: %w", err)
	}
	var s struct {
		Link      string `json:"link"`
		Endpoint  string `json:"endpoint"`
		Token     string `json:"token"`
		SessionID string `json:"sessionId"`
		Session2  string `json:"session_id"`
		Cwd       string `json:"cwd"`
	}
	_ = json.Unmarshal(data, &s)
	link := firstNonEmpty(s.Link, s.Endpoint)
	sid := firstNonEmpty(s.SessionID, s.Session2, conv)
	cwd := firstNonEmpty(s.Cwd, "/workspace")
	if link == "" || s.Token == "" {
		return nil, errors.New("沙箱未就绪（没有 link/token）")
	}
	res, err := c.intlRunTurn(ctx, a, conv, link, s.Token, sid, cwd, prompt, model)
	if err != nil {
		return res, err
	}
	keep = true
	return res, nil
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}

func (c *Client) intlWebAction(ctx context.Context, a *auth.Auth, conv, action string) bool {
	if conv == "" {
		return false
	}
	_, err := c.intlWebReq(ctx, a, http.MethodPost, c.intlConsoleBase(a)+url.PathEscape(conv)+"/"+action, map[string]any{})
	return err == nil
}

// intlWebPrune 名额满（每号最多 10 条进行中会话）时停掉本任务留下的最老几条。
func (c *Client) intlWebPrune(ctx context.Context, a *auth.Auth, prompt string) int {
	data, err := c.intlWebReq(ctx, a, http.MethodGet, c.intlConsoleBase(a)+"?page=1&pageSize=50", nil)
	if err != nil {
		return 0
	}
	var d struct {
		Conversations []map[string]any `json:"conversations"`
	}
	_ = json.Unmarshal(data, &d)
	n := 0
	for i := len(d.Conversations) - 1; i >= 0 && n < 3; i-- {
		cv := d.Conversations[i]
		st := fmt.Sprint(cv["status"])
		if st != "working" && st != "CREATING" && st != "creating" {
			continue
		}
		if fmt.Sprint(cv["title"]) != prompt && fmt.Sprint(cv["prompt"]) != prompt {
			continue
		}
		if c.intlWebAction(ctx, a, fmt.Sprint(cv["id"]), "stop") {
			n++
		}
	}
	return n
}

func (c *Client) intlWebStatus(ctx context.Context, a *auth.Auth, conv string) string {
	data, err := c.intlWebReq(ctx, a, http.MethodGet, c.intlConsoleBase(a)+url.PathEscape(conv), nil)
	if err != nil {
		return ""
	}
	var d struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(data, &d)
	return d.Status
}

func (c *Client) intlRunTurn(ctx context.Context, a *auth.Auth, conv, link, token, sid, cwd, prompt, model string) (*IntlWebResult, error) {
	started := time.Now()
	res := &IntlWebResult{Conversation: conv}
	ctx, cancel := context.WithTimeout(ctx, IntlWebTurnWait+30*time.Second)
	defer cancel()
	hc := c.ChatHTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return res, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", IntlWebUA)
	stream, err := hc.Do(req)
	if err != nil {
		return res, fmt.Errorf("SSE 通道: %w", err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != 200 {
		return res, fmt.Errorf("SSE 通道返回 HTTP %d", stream.StatusCode)
	}
	connID := stream.Header.Get("Acp-Connection-Id")
	if connID == "" {
		return res, errors.New("SSE 通道没有返回 Acp-Connection-Id")
	}
	var events, chunks atomic.Int64
	go func() {
		sc := bufio.NewScanner(stream.Body)
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var m struct {
				Method string `json:"method"`
				Params struct {
					Update struct {
						Kind string `json:"sessionUpdate"`
					} `json:"update"`
				} `json:"params"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &m) != nil {
				continue
			}
			if m.Method == "session/update" {
				events.Add(1)
				if m.Params.Update.Kind == "agent_message_chunk" {
					chunks.Add(1)
				}
			}
		}
	}()
	rpc := func(id int, method string, params any) error {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		rctx, rcancel := context.WithTimeout(ctx, 30*time.Second)
		defer rcancel()
		pr, err := http.NewRequestWithContext(rctx, http.MethodPost, link, bytes.NewReader(b))
		if err != nil {
			return err
		}
		pr.Header.Set("Content-Type", "application/json")
		pr.Header.Set("Accept", "application/json, text/event-stream")
		pr.Header.Set("Acp-Connection-Id", connID)
		pr.Header.Set("Authorization", "Bearer "+token)
		pr.Header.Set("User-Agent", IntlWebUA)
		resp, err := hc.Do(pr)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 && resp.StatusCode != 202 {
			return fmt.Errorf("%s 返回 HTTP %d", method, resp.StatusCode)
		}
		return nil
	}
	caps := map[string]any{"fs": map[string]bool{"readTextFile": false, "writeTextFile": false}, "terminal": false}
	if err := rpc(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": caps}); err != nil {
		return res, err
	}
	if err := rpc(2, "session/load", map[string]any{"sessionId": sid, "cwd": cwd, "mcpServers": []any{}}); err != nil {
		return res, err
	}
	if model != "" {
		_ = rpc(3, "session/set_model", map[string]any{"sessionId": sid, "modelId": model})
	}
	if err := rpc(4, "session/prompt", map[string]any{"sessionId": sid, "prompt": []map[string]string{{"type": "text", "text": prompt}}}); err != nil {
		return res, err
	}
	deadline := time.Now().Add(IntlWebTurnWait)
	for time.Now().Before(deadline) {
		st := c.intlWebStatus(ctx, a, conv)
		res.Status = st
		if st == "completed" || st == "failed" || st == "error" {
			break
		}
		select {
		case <-ctx.Done():
			deadline = time.Now()
		case <-time.After(intlWebPoll):
		}
	}
	res.Chunks, res.Events = chunks.Load(), events.Load()
	res.ElapsedMS = time.Since(started).Milliseconds()
	if res.Status != "completed" {
		return res, fmt.Errorf("会话在 %s 内没有跑完（状态=%s）", IntlWebTurnWait, firstNonEmpty(res.Status, "未知"))
	}
	return res, nil
}

// BuddyBoxResult 开盲盒结果。
type BuddyBoxResult struct {
	Opened int    `json:"opened"`
	Detail string `json:"detail,omitempty"`
}

// OpenBuddyBoxes 旅行结束后用多余能量开 Buddy 盲盒：quota.affordable 个（不超过 max_open_count）。
// affordable=0 返回 Opened=0、nil。
func (c *Client) OpenBuddyBoxes(a *auth.Auth) (*BuddyBoxResult, error) {
	data, err := c.growthJSON(a, http.MethodGet, "/v2/activity/growth/buddy/quota", nil)
	if err != nil {
		return nil, fmt.Errorf("能量额度查询失败: %w", err)
	}
	var q struct {
		Affordable int `json:"affordable"`
		MaxOpen    int `json:"max_open_count"`
	}
	_ = json.Unmarshal(data, &q)
	if q.Affordable <= 0 {
		return &BuddyBoxResult{}, nil
	}
	n := q.Affordable
	if q.MaxOpen > 0 && n > q.MaxOpen {
		n = q.MaxOpen
	}
	tok := make([]byte, 16)
	_, _ = rand.Read(tok)
	data, err = c.growthJSON(a, http.MethodPost, "/v2/activity/growth/buddy/open", map[string]any{"count": n, "client_token": hex.EncodeToString(tok)})
	if err != nil {
		return nil, fmt.Errorf("开 Buddy 盲盒失败: %w", err)
	}
	var d map[string]any
	_ = json.Unmarshal(data, &d)
	detail := ""
	for _, k := range []string{"buddy", "name", "buddies"} {
		if v, ok := d[k]; ok && v != nil {
			detail = truncate(fmt.Sprint(v), 120)
			break
		}
	}
	return &BuddyBoxResult{Opened: n, Detail: detail}, nil
}
