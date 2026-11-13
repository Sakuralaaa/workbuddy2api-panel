// v3.go panel-v3 装配辅助：调度扩展参数、面板「测试模型」的进程内请求。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panel"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/server"
)

// extrasFrom 把配置里 panel-v3 新增的排程键转成调度器参数。
func extrasFrom(c *Config) scheduler.Extras {
	return scheduler.Extras{
		Enabled:          c.Schedule.Enabled,
		IntlDailyEnabled: c.Schedule.IntlDailyEnabled,
		IntlDailyHours:   c.Schedule.IntlDailyHours,
		IntlDailyWeb:     c.Schedule.IntlDailyWeb,
		IntlDailyModels:  c.Schedule.IntlDailyModels,
		IntlDailyPrompt:  c.Schedule.IntlDailyPrompt,
		TravelOpenBoxes:  c.Schedule.TravelOpenBoxes,
	}
}

// liveFrom 热生效快照（启动与面板保存共用，避免两处漂移）。
func liveFrom(c *Config) livecfg.Snapshot {
	return livecfg.Snapshot{
		APIKey:               c.APIKey,
		AdminKey:             c.AdminKey,
		SoftCooldown:         c.SoftRateDur,
		SanitizeFingerprints: c.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     c.Logging.RequestClientInfo,
		HiddenModels:         livecfg.NormalizeHiddenModels(c.Models.Hidden),
		DisabledModels:       livecfg.NormalizeHiddenModels(c.Models.Disabled),
	}
}

// modelTester 返回面板「测试模型」实现：经 server.WithInternal 在进程内调用真实的
// /v1/chat/completions 处理链（选号 / 改写 / 上游 / 计量全同真实请求），不经网络、不需要密钥。
func modelTester(h *http.Handler) func(ctx context.Context, model string) panel.ModelTestResult {
	return func(ctx context.Context, model string) panel.ModelTestResult {
		res := panel.ModelTestResult{Model: model}
		if h == nil || *h == nil {
			res.Error = "handler not ready"
			return res
		}
		body, _ := json.Marshal(map[string]any{
			"model":      model,
			"messages":   []map[string]any{{"role": "user", "content": "Reply with the single word: OK"}},
			"max_tokens": 16,
			"stream":     false,
		})
		req, err := http.NewRequestWithContext(server.WithInternal(ctx), http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			res.Error = err.Error()
			return res
		}
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "127.0.0.1:0"
		rec := httptest.NewRecorder()
		start := time.Now()
		(*h).ServeHTTP(rec, req)
		res.LatencyMs = time.Since(start).Milliseconds()
		res.Status = rec.Code
		var out struct {
			Choices []struct {
				Message struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"message"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
				Code    any    `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		switch {
		case out.Error != nil:
			res.Error = strings.TrimSpace(fmt.Sprintf("%v %s", out.Error.Code, out.Error.Message))
		case rec.Code >= 300:
			res.Error = fmt.Sprintf("HTTP %d: %.200s", rec.Code, rec.Body.String())
		case len(out.Choices) > 0:
			res.OK = true
			res.Reply = out.Choices[0].Message.Content
			if res.Reply == "" {
				res.Reply = out.Choices[0].Message.ReasoningContent
			}
			if r := []rune(res.Reply); len(r) > 200 {
				res.Reply = string(r[:200]) + "…"
			}
		default:
			res.Error = fmt.Sprintf("无法解析的响应: %.200s", rec.Body.String())
		}
		return res
	}
}
