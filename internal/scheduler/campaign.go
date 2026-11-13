// campaign.go 限时活动任务（星巴克协作 / 记者节 等 /portal/activity/* 运营活动）。
//
// 每轮：对每个国内版个人账号收集候选活动 code（内置已知 + schedule.activity_codes 配置 +
// 成长任务清单原始响应里扫到的 code + 以前见过的 code），逐个读任务接口，非空即「已下发」。
// 第一次出现的 code 记为「新活动」（面板显示角标，直到点开确认）。执行模式下对可纯 API 完成的
// 任务（chat / expert / skill / desktop）上报判据事件、轮询进度、领奖；需真人的任务只列出不硬跑。
// 快照（每账号活动列表 + 已见 code）落盘到 activities.json，重启保留。
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// JobCampaign 限时活动任务 job id。
const JobCampaign = "campaign"

func init() { JobNames[JobCampaign] = "限时活动" }

// 判据轮询节奏（测试置 0）。
var (
	campaignPollGap   = 3 * time.Second
	campaignPollTries = 5
)

// CampaignAccount 单账号活动快照。
type CampaignAccount struct {
	UID        string              `json:"uid"`
	Nickname   string              `json:"nickname,omitempty"`
	CheckedAt  time.Time           `json:"checked_at"`
	Error      string              `json:"error,omitempty"`
	Activities []upstream.Activity `json:"activities"`
}

// CampaignSeen 一个活动 code 的首次发现记录。
type CampaignSeen struct {
	Name      string    `json:"name"`
	FirstSeen time.Time `json:"first_seen"`
	New       bool      `json:"new"` // 未在面板确认过
}

type campaignFile struct {
	Accounts map[string]*CampaignAccount `json:"accounts"`
	Seen     map[string]*CampaignSeen    `json:"seen"`
	LastScan time.Time                   `json:"last_scan"`
}

type campaignState struct {
	mu   sync.Mutex
	path string
	d    campaignFile
}

func (c *campaignState) init() {
	if c.d.Accounts == nil {
		c.d.Accounts = map[string]*CampaignAccount{}
	}
	if c.d.Seen == nil {
		c.d.Seen = map[string]*CampaignSeen{}
	}
}

// SetCampaignFile 设置活动快照文件并读入（不存在 = 空）。
func (s *Scheduler) SetCampaignFile(path string) error {
	s.camp.mu.Lock()
	defer s.camp.mu.Unlock()
	s.camp.path = path
	s.camp.init()
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var d campaignFile
	if err := json.Unmarshal(raw, &d); err != nil {
		return err
	}
	s.camp.d = d
	s.camp.init()
	return nil
}

// saveLocked 落盘（调用方持锁）。
func (s *Scheduler) saveCampaignLocked() {
	if s.camp.path == "" {
		return
	}
	raw, err := json.MarshalIndent(s.camp.d, "", " ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.camp.path), 0o755)
	tmp := s.camp.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err == nil {
		_ = os.Rename(tmp, s.camp.path)
	}
}

// CampaignSnapshot 面板读取：每账号活动 + 已见 code（拷贝）。
type CampaignSnapshotData struct {
	Accounts []CampaignAccount       `json:"accounts"`
	Seen     map[string]CampaignSeen `json:"seen"`
	NewCount int                     `json:"new_count"`
	LastScan time.Time               `json:"last_scan"`
}

// CampaignSnapshot 当前活动快照。
func (s *Scheduler) CampaignSnapshot() CampaignSnapshotData {
	s.camp.mu.Lock()
	defer s.camp.mu.Unlock()
	s.camp.init()
	out := CampaignSnapshotData{Seen: map[string]CampaignSeen{}, LastScan: s.camp.d.LastScan}
	for code, v := range s.camp.d.Seen {
		out.Seen[code] = *v
		if v.New {
			out.NewCount++
		}
	}
	for _, a := range s.camp.d.Accounts {
		out.Accounts = append(out.Accounts, *a)
	}
	sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i].UID < out.Accounts[j].UID })
	return out
}

// MarkCampaignsSeen 面板确认新活动（清角标）。
func (s *Scheduler) MarkCampaignsSeen() {
	s.camp.mu.Lock()
	defer s.camp.mu.Unlock()
	s.camp.init()
	for _, v := range s.camp.d.Seen {
		v.New = false
	}
	s.saveCampaignLocked()
}

// campaignEligible 只有国内版个人账号有运营活动。
func campaignEligible(a *auth.Auth) bool {
	return a != nil && a.AccessTokenValue() != "" && !a.IsGlobal() && !a.IsEnterpriseSession() && a.EnterpriseID == ""
}

// campaignCandidates 候选活动 code：已知 + 配置 + 已见 + 本账号清单扫到的。
func (s *Scheduler) campaignCandidates(a *auth.Auth) []string {
	set := map[string]bool{}
	for _, k := range upstream.KnownActivities {
		set[k.Code] = true
	}
	for _, c := range s.GetExtras().ActivityCodes {
		if c = strings.TrimSpace(c); c != "" {
			set[c] = true
		}
	}
	s.camp.mu.Lock()
	for c := range s.camp.d.Seen {
		set[c] = true
	}
	s.camp.mu.Unlock()
	for _, c := range s.cfg.Upstream.DiscoverActivityCodes(a) {
		set[c] = true
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// scanCampaigns 单账号读全部候选活动，返回已下发的活动与首个读失败。
func (s *Scheduler) scanCampaigns(a *auth.Auth) ([]upstream.Activity, error) {
	var acts []upstream.Activity
	var firstErr error
	okReads := 0
	for _, code := range s.campaignCandidates(a) {
		act, err := s.cfg.Upstream.ActivityTasks(a, code)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %v", code, err)
			}
			continue
		}
		okReads++
		if len(act.Tasks) > 0 {
			acts = append(acts, act)
		}
	}
	if okReads > 0 {
		firstErr = nil // 个别 code 不存在 / 已下线返回错误很常见，只要有成功读到的就不算失败
	}
	return acts, firstErr
}

// storeCampaigns 记录单账号快照与新活动；返回本次新发现的 code。
func (s *Scheduler) storeCampaigns(a *auth.Auth, acts []upstream.Activity, err error) []string {
	s.camp.mu.Lock()
	defer s.camp.mu.Unlock()
	s.camp.init()
	acc := &CampaignAccount{UID: a.UID, Nickname: a.Nickname, CheckedAt: time.Now(), Activities: acts}
	if acts == nil {
		acc.Activities = []upstream.Activity{}
	}
	if err != nil {
		acc.Error = err.Error()
		if old := s.camp.d.Accounts[a.UID]; old != nil && len(acts) == 0 {
			acc.Activities = old.Activities // 读失败保留上次结果
		}
	}
	s.camp.d.Accounts[a.UID] = acc
	var fresh []string
	for _, act := range acts {
		if _, ok := s.camp.d.Seen[act.Code]; !ok {
			s.camp.d.Seen[act.Code] = &CampaignSeen{Name: act.Name, FirstSeen: time.Now(), New: true}
			fresh = append(fresh, act.Code)
		}
	}
	s.camp.d.LastScan = time.Now()
	s.saveCampaignLocked()
	return fresh
}

// campaignPoll 轮询任务进度直到达标或次数用尽。
func (s *Scheduler) campaignPoll(a *auth.Auth, code, task string, target int) (upstream.ActivityTask, bool) {
	var last upstream.ActivityTask
	for i := 0; i < campaignPollTries; i++ {
		time.Sleep(campaignPollGap)
		act, err := s.cfg.Upstream.ActivityTasks(a, code)
		if err != nil {
			continue
		}
		for _, t := range act.Tasks {
			if t.Code == task {
				last = t
				if t.Completed || t.Claimed || t.Progress >= target {
					return t, true
				}
			}
		}
	}
	return last, false
}

// campaignRunTask 执行单个任务；返回 (领到的积分, 说明, 是否失败, 是否需手动)。
func (s *Scheduler) campaignRunTask(a *auth.Auth, act upstream.Activity, t upstream.ActivityTask) (float64, string, bool, bool) {
	label := act.Name + " / " + t.Name
	if t.Claimed {
		return 0, label + "：已领奖", false, false
	}
	done := t.Status() == "claimable"
	if !done {
		if t.Kind == upstream.ActivityKindManual {
			return 0, label + "：需手动完成", false, true
		}
		if t.NeedView {
			_ = s.cfg.Upstream.ActivityViewed(a, act.Code, t.Code)
		}
		times := t.Target - t.Progress
		if times < 1 {
			times = 1
		}
		if times > 10 {
			times = 10
		}
		for i := 0; i < times; i++ {
			if err := s.cfg.Upstream.FireActivityTask(a, act, t); err != nil {
				return 0, fmt.Sprintf("%s：上报失败 %v", label, err), true, false
			}
		}
		cur, ok := s.campaignPoll(a, act.Code, t.Code, t.Target)
		if !ok {
			return 0, fmt.Sprintf("%s：判据未落库（%d/%d），下轮重试", label, cur.Progress, t.Target), false, false
		}
		if cur.Claimed {
			return 0, label + "：已领奖", false, false
		}
	}
	if err := s.cfg.Upstream.ActivityClaim(a, act.Code, t.Code); err != nil {
		return 0, fmt.Sprintf("%s：领奖失败 %v", label, err), true, false
	}
	return float64(t.Credit), fmt.Sprintf("%s：领奖 +%d", label, t.Credit), false, false
}

// campaignOne 单账号：扫描活动，execute=true 时执行可完成任务并领奖，记一条任务记录。
func (s *Scheduler) campaignOne(a *auth.Auth, execute bool) JobResult {
	if !campaignEligible(a) {
		return s.note(JobCampaign, a, true, "skipped", "限时活动只适用于国内版个人账号", 0)
	}
	acts, err := s.scanCampaigns(a)
	fresh := s.storeCampaigns(a, acts, err)
	if err != nil {
		log.Printf("campaign %s: %v", logfmt.Label(a.UID, a.Nickname), err)
		return s.noteErr(JobCampaign, a, err)
	}
	if len(fresh) > 0 {
		log.Printf("campaign %s: 发现新活动 %v", logfmt.Label(a.UID, a.Nickname), fresh)
	}
	if len(acts) == 0 {
		return s.note(JobCampaign, a, true, "skipped", "当前没有下发的限时活动", 0)
	}
	if !execute {
		pending := 0
		for _, act := range acts {
			for _, t := range act.Tasks {
				if !t.Claimed {
					pending++
				}
			}
		}
		msg := fmt.Sprintf("发现 %d 个活动，%d 个任务未领奖", len(acts), pending)
		if len(fresh) > 0 {
			msg += "（新活动 " + strings.Join(fresh, "、") + "）"
		}
		return s.note(JobCampaign, a, true, "done", msg, 0)
	}
	var credits float64
	var lines []string
	failed, manual := 0, 0
	for _, act := range acts {
		for _, t := range act.Tasks {
			c, msg, bad, man := s.campaignRunTask(a, act, t)
			credits += c
			if bad {
				failed++
			}
			if man {
				manual++
			}
			if c > 0 || bad {
				lines = append(lines, msg)
			}
		}
	}
	// 执行后回读一次，面板状态与上游一致。
	if acts2, err2 := s.scanCampaigns(a); err2 == nil {
		s.storeCampaigns(a, acts2, nil)
	}
	summary := fmt.Sprintf("限时活动 %d 个，+%.0f 积分", len(acts), credits)
	if manual > 0 {
		summary += fmt.Sprintf("，%d 个需手动", manual)
	}
	if len(lines) > 0 {
		summary += "；" + strings.Join(lines, "；")
	}
	log.Printf("campaign %s: %s", logfmt.Label(a.UID, a.Nickname), summary)
	switch {
	case failed > 0:
		return s.note(JobCampaign, a, false, "failed", summary, credits)
	case credits > 0:
		return s.note(JobCampaign, a, true, "claimed", summary, credits)
	}
	return s.note(JobCampaign, a, true, "done", summary, credits)
}

func (s *Scheduler) runCampaign(ctx context.Context, execute bool) {
	first := true
	includeDisabled := s.includeDisabledInTasks()
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled && !includeDisabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if !campaignEligible(a) {
			continue
		}
		if !first && !sleepCtx(ctx, activityAccountDelay) {
			return
		}
		first = false
		s.campaignOne(a, execute)
	}
}

// RunCampaignNow 全部账号：发现 + 执行 + 领奖。
func (s *Scheduler) RunCampaignNow() {
	s.track(JobCampaign, "manual", func() { s.runCampaign(context.Background(), true) })
}

// DiscoverCampaignsNow 全部账号：只发现、不执行（面板「发现新活动」）。
func (s *Scheduler) DiscoverCampaignsNow(ctx context.Context) {
	s.track(JobCampaign, "manual", func() { s.runCampaign(ctx, false) })
}
