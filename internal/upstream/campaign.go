// campaign.go 限时运营活动（/portal/activity/* 契约，如「星巴克协作」「记者节福利活动」）。
//
// 契约（对齐 WorkBuddy-plugins internal/jobs/campaigns.go 与 hub wb_tasks.run_campaign_tasks）：
//   - GET  /portal/activity/tasks?activity_code=<code>        读任务定义 + 进度（data.tasks / data.progress）
//   - POST /portal/activity/tasks/<task>/viewed?activity_code= 标记已查看（need_view 任务）
//   - POST /portal/activity/tasks/<task>/claim?activity_code=  领奖
//
// 上游没有公开的「活动列表」接口（插件 / hub 都是写死活动 code），所以发现活动的口径是：
// 内置已知 code + 配置追加的 code + 从成长任务清单原始响应里扫出的 activity_code / campaign_* 字样；
// 对每个候选 code 读一次任务接口，返回非空任务即视为该账号「已下发」的活动。
// 任务按 task_code / 任务类型字段归类为 chat / expert / skill / desktop（可纯 API 完成）或
// manual（需真人，例如手机 App 对话、分享），归不了类的一律 manual，不硬跑。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// ActivityTaskKind 任务的完成方式。
const (
	ActivityKindChat    = "chat"
	ActivityKindExpert  = "expert"
	ActivityKindSkill   = "skill"
	ActivityKindDesktop = "desktop"
	ActivityKindManual  = "manual"
)

// KnownActivity 已知活动的补充信息（名称、小程序页、任务覆盖）。未知活动只靠上游字段。
type KnownActivity struct {
	Code  string
	Name  string
	Page  map[string]any
	Tasks map[string]KnownActivityTask
}

// KnownActivityTask 已知任务的覆盖：完成方式、专家 / 技能 / 案例参数。
type KnownActivityTask struct {
	Label      string
	Kind       string
	ExpertID   string
	ExpertName string
	SkillID    string
	SkillName  string
	CaseID     string
}

// KnownActivities 已知活动（种子）。新活动不需要加在这里：发现流程会把扫到的 code 当未知活动处理。
var KnownActivities = []KnownActivity{
	{
		Code: "campaign_starbucks_2026", Name: "星巴克协作",
		Page: map[string]any{"pageURL": "subpackages/secondary/starbucks-activity-428/index", "pageName": "starbucks_activity"},
		Tasks: map[string]KnownActivityTask{
			"ai_chat": {Label: "与 AI 对话 3 次", Kind: ActivityKindChat},
			"expert":  {Label: "召唤专家并对话", Kind: ActivityKindExpert},
			"app":     {Label: "解锁 App 强大能力（需手机 App 真实对话）", Kind: ActivityKindManual},
		},
	},
	{
		Code: "campaign_reporter_2026", Name: "记者节福利活动",
		Page: map[string]any{"pageURL": "subpackages/journalist-benefit/index", "pageName": "journalist_benefit"},
		Tasks: map[string]KnownActivityTask{
			"expert_chat":  {Label: "体验「媒体工作助理」专家", Kind: ActivityKindExpert, ExpertID: "ex_NaiDbO06Adf3", ExpertName: "媒体工作助理"},
			"skill_use":    {Label: "体验「采访素材整理」技能", Kind: ActivityKindSkill, SkillID: "skill_2117491159283752960", SkillName: "interview-material-organizer"},
			"desktop_same": {Label: "体验「内容发布排期」灵感", Kind: ActivityKindDesktop, CaseID: "content-calendar"},
		},
	},
}

// activityCases 灵感案例（活动判据认产物目录名）。
var activityCases = map[string]map[string]any{
	"content-calendar": {"id": "cnb-content-calendar", "name": "内容发布排期表", "type": "table", "promptLength": 359},
}

// LookupActivity 按 code 查已知活动。
func LookupActivity(code string) (KnownActivity, bool) {
	for _, k := range KnownActivities {
		if k.Code == code {
			return k, true
		}
	}
	return KnownActivity{}, false
}

// ActivityTask 一个活动任务的定义 + 进度。
type ActivityTask struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Type      string `json:"type,omitempty"` // 上游任务类型字段原值（task_type / action / type）
	Kind      string `json:"kind"`           // chat / expert / skill / desktop / manual
	Target    int    `json:"target"`
	Progress  int    `json:"progress"`
	Completed bool   `json:"completed"`
	Claimed   bool   `json:"claimed"`
	NeedView  bool   `json:"need_view"`
	Credit    int    `json:"credit"`
	Deadline  string `json:"deadline,omitempty"`

	ExpertID   string `json:"expert_id,omitempty"`
	ExpertName string `json:"expert_name,omitempty"`
	SkillID    string `json:"skill_id,omitempty"`
	SkillName  string `json:"skill_name,omitempty"`
	CaseID     string `json:"case_id,omitempty"`
}

// Status 面板展示状态：claimed / claimable / pending / manual。
func (t ActivityTask) Status() string {
	switch {
	case t.Claimed:
		return "claimed"
	case t.Completed || (t.Target > 0 && t.Progress >= t.Target):
		return "claimable"
	case t.Kind == ActivityKindManual:
		return "manual"
	default:
		return "pending"
	}
}

// Activity 一个已下发活动（单账号视角）。
type Activity struct {
	Code     string         `json:"code"`
	Name     string         `json:"name"`
	Deadline string         `json:"deadline,omitempty"`
	Tasks    []ActivityTask `json:"tasks"`
}

// activityPath 活动接口 path（task 为空 = 列表）。
func activityPath(action, code, task string) string {
	if task == "" {
		return "/portal/activity/tasks?activity_code=" + url.QueryEscape(code)
	}
	return fmt.Sprintf("/portal/activity/tasks/%s/%s?activity_code=%s", url.PathEscape(task), action, url.QueryEscape(code))
}

func (c *Client) activityCall(a *auth.Auth, method, path string) (json.RawMessage, error) {
	var body *bytes.Reader
	if method == http.MethodPost {
		body = bytes.NewReader([]byte("{}"))
	} else {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.billingBase(a)+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	req.Header.Set("X-Client-Product", "workbuddy-mp")
	req.Header.Set("X-Client-Platform", "mp-weixin")
	req.Header.Set("X-Platform", "wechatmp")
	return c.doJSON(req)
}

// rawSet 时间戳类字段是否「已设置」（null / "" / 0 视为未设置）。
func rawSet(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != `""` && s != "0" && s != "false"
}

func pickStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case float64:
			if v != 0 {
				return fmt.Sprintf("%.0f", v)
			}
		}
	}
	return ""
}

func pickNum(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return v, true
		case string:
			var f float64
			if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

// normDeadline 把秒 / 毫秒时间戳或时间串统一成 RFC3339（CST）；认不出的原样返回。
func normDeadline(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && fmt.Sprint(n) == s {
		if n > 1e12 {
			n /= 1000
		}
		if n > 1e9 {
			return time.Unix(n, 0).In(time.FixedZone("CST", 8*3600)).Format(time.RFC3339)
		}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.FixedZone("CST", 8*3600)); err == nil {
			return t.Format(time.RFC3339)
		}
	}
	return s
}

var deadlineKeys = []string{"end_time", "end_at", "endTime", "deadline", "expire_at", "expired_at", "end_date"}

// parseActivity 宽松解析 GET /portal/activity/tasks 的 data。
func parseActivity(code string, data json.RawMessage) (Activity, error) {
	act := Activity{Code: code}
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		if string(data) == "null" || len(bytes.TrimSpace(data)) == 0 {
			return act, nil
		}
		return act, fmt.Errorf("活动任务解析失败: %w", err)
	}
	act.Name = pickStr(top, "activity_name", "name", "title")
	act.Deadline = normDeadline(pickStr(top, deadlineKeys...))
	if info, ok := top["activity"].(map[string]any); ok {
		if act.Name == "" {
			act.Name = pickStr(info, "activity_name", "name", "title")
		}
		if act.Deadline == "" {
			act.Deadline = normDeadline(pickStr(info, deadlineKeys...))
		}
	}
	var d struct {
		Tasks    []map[string]any `json:"tasks"`
		Progress []struct {
			Code        string          `json:"task_code"`
			Progress    float64         `json:"progress"`
			CompletedAt json.RawMessage `json:"completed_at"`
			ClaimedAt   json.RawMessage `json:"claimed_at"`
		} `json:"progress"`
	}
	_ = json.Unmarshal(data, &d)
	type prog struct {
		p          int
		done, took bool
	}
	pm := map[string]prog{}
	for _, p := range d.Progress {
		pm[p.Code] = prog{int(p.Progress), rawSet(p.CompletedAt), rawSet(p.ClaimedAt)}
	}
	known, _ := LookupActivity(code)
	if act.Name == "" {
		act.Name = known.Name
	}
	for _, m := range d.Tasks {
		t := ActivityTask{Code: pickStr(m, "task_code", "code")}
		if t.Code == "" {
			continue
		}
		t.Name = pickStr(m, "task_name", "name", "title", "task_title")
		t.Type = pickStr(m, "task_type", "action_type", "action", "type", "category")
		if v, ok := pickNum(m, "target_count", "target"); ok {
			t.Target = int(v)
		}
		if v, ok := pickNum(m, "reward_credit", "credit", "reward"); ok {
			t.Credit = int(v)
		}
		t.NeedView = true
		if b, ok := m["need_view"].(bool); ok {
			t.NeedView = b
		}
		t.Deadline = normDeadline(pickStr(m, deadlineKeys...))
		ext := map[string]any{}
		for _, k := range []string{"ext", "extra", "config", "params"} {
			if e, ok := m[k].(map[string]any); ok {
				for kk, vv := range e {
					ext[kk] = vv
				}
			}
		}
		for k, v := range m {
			ext[k] = v
		}
		t.ExpertID = pickStr(ext, "expert_id", "expertId")
		t.ExpertName = pickStr(ext, "expert_name", "expertName")
		t.SkillID = pickStr(ext, "skill_id", "skillId")
		t.SkillName = pickStr(ext, "skill_name", "skillName")
		t.CaseID = pickStr(ext, "case_id", "caseId", "playbook_id")
		if p, ok := pm[t.Code]; ok {
			t.Progress, t.Completed, t.Claimed = p.p, p.done, p.took
		}
		if v, ok := m["completed"].(bool); ok && v {
			t.Completed = true
		}
		if v, ok := m["claimed"].(bool); ok && v {
			t.Claimed = true
		}
		if kt, ok := known.Tasks[t.Code]; ok {
			if t.Name == "" {
				t.Name = kt.Label
			}
			t.Kind = kt.Kind
			if t.ExpertID == "" {
				t.ExpertID, t.ExpertName = kt.ExpertID, kt.ExpertName
			}
			if t.SkillID == "" {
				t.SkillID, t.SkillName = kt.SkillID, kt.SkillName
			}
			if t.CaseID == "" {
				t.CaseID = kt.CaseID
			}
		}
		if t.Kind == "" {
			t.Kind = ClassifyActivityTask(t)
		}
		if t.Name == "" {
			t.Name = t.Code
		}
		if t.Target <= 0 {
			t.Target = 1
		}
		if act.Deadline == "" && t.Deadline != "" {
			act.Deadline = t.Deadline
		}
		act.Tasks = append(act.Tasks, t)
	}
	if act.Name == "" {
		act.Name = code
	}
	return act, nil
}

// ClassifyActivityTask 未知任务按字段 / 关键字归类；归不了类的返回 manual（不硬跑）。
func ClassifyActivityTask(t ActivityTask) string {
	if t.CaseID != "" {
		if _, ok := activityCases[t.CaseID]; ok {
			return ActivityKindDesktop
		}
		return ActivityKindManual
	}
	if t.SkillID != "" {
		return ActivityKindSkill
	}
	if t.ExpertID != "" {
		return ActivityKindExpert
	}
	s := strings.ToLower(t.Type + " " + t.Code)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(s, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("app", "share", "invite", "subscribe", "follow", "download", "upload", "pay", "order", "sign_up", "signup", "photo"):
		return ActivityKindManual
	case has("expert"):
		return ActivityKindExpert
	case has("chat", "dialog", "conversation", "talk", "message"):
		return ActivityKindChat
	}
	return ActivityKindManual
}

// ActivityTasks 读一个活动在该账号下的任务；空任务 = 未下发或已结束。
func (c *Client) ActivityTasks(a *auth.Auth, code string) (Activity, error) {
	data, err := c.activityCall(a, http.MethodGet, activityPath("", code, ""))
	if err != nil {
		return Activity{Code: code}, err
	}
	return parseActivity(code, data)
}

// ActivityViewed 标记任务已查看（失败不致命）。
func (c *Client) ActivityViewed(a *auth.Auth, code, task string) error {
	_, err := c.activityCall(a, http.MethodPost, activityPath("viewed", code, task))
	return err
}

// ActivityClaim 领取任务奖励。
func (c *Client) ActivityClaim(a *auth.Auth, code, task string) error {
	_, err := c.activityCall(a, http.MethodPost, activityPath("claim", code, task))
	return err
}

var (
	reActivityCodeQS   = regexp.MustCompile(`activity_code=([A-Za-z0-9_\-]{3,64})`)
	reActivityCodeJSON = regexp.MustCompile(`"(?:activity_code|activityCode)"\s*:\s*"([A-Za-z0-9_\-]{3,64})"`)
	reCampaignWord     = regexp.MustCompile(`\bcampaign_[a-z0-9_]{3,60}\b`)
)

// HarvestActivityCodes 从任意上游原始响应里扫出活动 code（去重、排序）。
func HarvestActivityCodes(raw []byte) []string {
	set := map[string]bool{}
	for _, re := range []*regexp.Regexp{reActivityCodeQS, reActivityCodeJSON} {
		for _, m := range re.FindAllSubmatch(raw, -1) {
			set[string(m[1])] = true
		}
	}
	for _, m := range reCampaignWord.FindAll(raw, -1) {
		set[string(m)] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DiscoverActivityCodes 从成长任务清单（默认 + 小程序口径）原始响应里扫活动 code。
// 读失败只是少一个来源，不报错。
func (c *Client) DiscoverActivityCodes(a *auth.Auth) []string {
	set := map[string]bool{}
	if raw, err := c.growthJSON(a, http.MethodGet, tasksListPath, nil); err == nil {
		for _, k := range HarvestActivityCodes(raw) {
			set[k] = true
		}
	}
	if raw, err := c.growthJSONMP(a, http.MethodGet, tasksListPath, nil); err == nil {
		for _, k := range HarvestActivityCodes(raw) {
			set[k] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func activityMPEvent(a *auth.Auth, page map[string]any, ev map[string]any) map[string]any {
	for k, v := range page {
		if _, has := ev[k]; !has {
			ev[k] = v
		}
	}
	return ev
}

// FireActivityTask 按任务类型上报一次判据事件（chat 一次计 1 次进度）。manual 返回错误。
func (c *Client) FireActivityTask(a *auth.Auth, act Activity, t ActivityTask) error {
	known, _ := LookupActivity(act.Code)
	page := known.Page
	conv := fmt.Sprintf("wb2api-act-%d", time.Now().UnixNano())
	switch t.Kind {
	case ActivityKindChat:
		return c.ReportMPEvent(a, activityMPEvent(a, page, SchoolChatTimesEvents(conv)))
	case ActivityKindExpert:
		id, name := t.ExpertID, t.ExpertName
		if id == "" {
			if list, err := c.MarketExpertList(a, "agent"); err == nil && len(list) > 0 {
				id, name = list[0].ExpertID, list[0].DisplayNameZH
			}
		}
		if id == "" {
			return fmt.Errorf("没有可用的专家 id")
		}
		if name == "" {
			name = id
		}
		const cat = "16-BackToSchool"
		chat := SchoolChatTimesEvents(conv)
		chat["expertId"], chat["expertName"] = id, name
		return c.ReportMPEvent(a,
			activityMPEvent(a, page, map[string]any{"eventCode": "expert_summon_click", "id": id, "name": id, "expertTitle": name, "type": cat, "position": 0}),
			activityMPEvent(a, page, map[string]any{"eventCode": "expert_summoned", "id": id, "name": id, "expertTitle": name}),
			activityMPEvent(a, page, map[string]any{"eventCode": "expert_actual_use", "id": id, "name": id, "expertTitle": name, "type": cat, "characterCount": 14, "expertType": "builtin"}),
			activityMPEvent(a, page, chat),
		)
	case ActivityKindSkill:
		if t.SkillID == "" {
			return fmt.Errorf("任务没有技能 id，需手动")
		}
		name := t.SkillName
		if name == "" {
			name = t.SkillID
		}
		chat := SchoolChatTimesEvents(conv)
		chat["skillId"], chat["skillCount"] = t.SkillID, 1
		return c.ReportMPEvent(a,
			activityMPEvent(a, page, map[string]any{"eventCode": "skill_info", "id": name, "skillId": t.SkillID, "skillName": name, "skillType": "market", "skillSource": "market", "position": 0, "conversationId": conv}),
			activityMPEvent(a, page, chat),
		)
	case ActivityKindDesktop:
		cs, ok := activityCases[t.CaseID]
		if !ok {
			return fmt.Errorf("未内置灵感案例 %s，需手动", t.CaseID)
		}
		ev := DesktopEvent{"eventCode": "playbook_prompt_send", "isOfficial": 1, "source": "discover", "query": "", "ext1": "discover",
			"requestId": "wb2api-" + clientToken(), "conversationId": conv}
		for k, v := range cs {
			ev[k] = v
		}
		return c.ReportDesktopEvent(a, ev)
	}
	return fmt.Errorf("需要真人完成")
}
