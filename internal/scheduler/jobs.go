// jobs.go panel-v3：任务执行留痕（每账号任务记录 + 结构化运行历史）、调度总开关、
// 国际版每日活跃、旅行后开 Buddy 盲盒，以及面板「单账号执行 / 全部账号执行」入口。
//
// 留痕方式：各任务在原有日志点旁调用 s.note(...) 产出 JobResult；一次批量执行（排程到点
// 或面板「全部账号执行」）由 s.track 包裹成一个 RunReport。观察者（main 注入，落到
// panelstore 持久化）分别收到逐条结果与整轮汇总。未挂观察者时一切行为与旧版一致。
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
)

// 任务 id（与面板 / 记录里的 job 字段一致）。
const (
	JobCheckin    = "checkin"
	JobStreak     = "streak"
	JobTravel     = "travel"
	JobOpenBoxes  = "open_boxes"
	JobActivity   = "activity"
	JobKeepalive  = "keepalive"
	JobBlackcat   = "blackcat"
	JobIntlDaily  = "intl_daily"
	JobGrowth     = "growth"
	JobBalance    = "balance"
	JobTaskSweep  = "task_sweep"
	JobCompensate = "compensation"
)

// JobNames 任务展示名。
var JobNames = map[string]string{
	JobCheckin:    "每日签到",
	JobStreak:     "连登管家",
	JobTravel:     "猫猫旅行",
	JobOpenBoxes:  "开 Buddy 盲盒",
	JobActivity:   "活跃上报",
	JobKeepalive:  "Token 保活",
	JobBlackcat:   "夜猫子",
	JobIntlDaily:  "国际版每日活跃",
	JobGrowth:     "成长任务",
	JobBalance:    "余额刷新",
	JobTaskSweep:  "任务补领",
	JobCompensate: "官方补偿",
}

// parentJob 子步骤归属的批次（连登管家跑在签到批次里，开盲盒跑在旅行批次里）。
var parentJob = map[string]string{JobStreak: JobCheckin, JobOpenBoxes: JobTravel}

// JobResult 一个账号一次任务的结果。
type JobResult struct {
	RunID    string    `json:"run_id,omitempty"`
	Trigger  string    `json:"trigger"` // schedule / manual
	Job      string    `json:"job"`
	UID      string    `json:"uid"`
	Nickname string    `json:"nickname,omitempty"`
	OK       bool      `json:"ok"`
	Outcome  string    `json:"outcome"` // done / skipped / failed / claimed
	Message  string    `json:"message"`
	Credits  float64   `json:"credits,omitempty"`
	At       time.Time `json:"at"`
}

// RunReport 一轮批量执行的汇总（结构化运行历史）。
type RunReport struct {
	ID       string      `json:"id"`
	Job      string      `json:"job"`
	Trigger  string      `json:"trigger"`
	Started  time.Time   `json:"started"`
	Finished time.Time   `json:"finished"`
	Accounts int         `json:"accounts"`
	OK       int         `json:"ok"`
	Failed   int         `json:"failed"`
	Skipped  int         `json:"skipped"`
	Credits  float64     `json:"credits"`
	Results  []JobResult `json:"results,omitempty"`
}

// Observer 执行留痕的接收方。
type Observer interface {
	JobResult(JobResult)
	RunDone(RunReport)
}

type runTrack struct {
	rep RunReport
}

type jobState struct {
	mu       sync.Mutex
	observer Observer
	active   map[string]*runTrack // job → 进行中的批次
	intlDone map[string]string    // uid → 已成功的 CST 日（国际版每日活跃当日成功后不再重复）
}

// Extras panel-v3 新增的排程参数（独立 setter，避免继续加长 Reconfigure 的位置参数）。
type Extras struct {
	// Enabled 调度总开关（schedule.enabled，缺省 true）。false = 所有定时任务与任务补领
	// 清扫都不再自动运行（面板手动执行照常可用）；余额后台刷新不受影响（它是选号依据）。
	Enabled bool
	// IntlDailyEnabled 国际版每日活跃（schedule.intl_daily_enabled，缺省 false）。
	IntlDailyEnabled bool
	IntlDailyHours   []int    // 缺省 [10, 20]（当天成功一次后后续时点自动跳过）
	IntlDailyWeb     bool     // 同时跑网页通道（schedule.intl_daily_web）
	IntlDailyModels  []string // 桌面通道依次尝试的模型，缺省 ["deepseek-v4.1-flash"]
	IntlDailyPrompt  string   // 缺省 "Hi"
	// TravelOpenBoxes 旅行巡检后用多余能量开 Buddy 盲盒（schedule.travel_open_boxes，缺省 false）。
	TravelOpenBoxes bool
}

func newRunID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return time.Now().Format("0102-150405-") + hex.EncodeToString(b)[:6]
}

// SetObserver 挂载执行留痕接收方（nil = 不留痕）。
func (s *Scheduler) SetObserver(o Observer) {
	s.js.mu.Lock()
	s.js.observer = o
	s.js.mu.Unlock()
}

// SetExtras 热更新 panel-v3 排程参数并让主循环重算。
func (s *Scheduler) SetExtras(e Extras) {
	if len(e.IntlDailyHours) == 0 {
		e.IntlDailyHours = []int{10, 20}
	}
	s.schedMu.Lock()
	s.extras = e
	s.schedMu.Unlock()
	poke(s.rearmSchedule)
}

// GetExtras 当前 panel-v3 排程参数快照。
func (s *Scheduler) GetExtras() Extras {
	s.schedMu.Lock()
	defer s.schedMu.Unlock()
	return s.extras
}

// Enabled 调度总开关当前值。
func (s *Scheduler) Enabled() bool { return s.GetExtras().Enabled }

// track 把 fn 包成一轮批量执行；同一 job 已有进行中的批次时直接执行（嵌套调用归入外层批次）。
func (s *Scheduler) track(job, trigger string, fn func()) {
	s.js.mu.Lock()
	if s.js.active == nil {
		s.js.active = map[string]*runTrack{}
	}
	if _, busy := s.js.active[job]; busy {
		s.js.mu.Unlock()
		fn()
		return
	}
	rt := &runTrack{rep: RunReport{ID: newRunID(), Job: job, Trigger: trigger, Started: time.Now()}}
	s.js.active[job] = rt
	s.js.mu.Unlock()
	defer func() {
		s.js.mu.Lock()
		delete(s.js.active, job)
		rt.rep.Finished = time.Now()
		obs := s.js.observer
		rep := rt.rep
		s.js.mu.Unlock()
		if obs != nil {
			obs.RunDone(rep)
		}
	}()
	fn()
}

// note 记一条账号级结果（归入进行中的批次；没有批次时单独成条，trigger=manual）。
func (s *Scheduler) note(job string, a *auth.Auth, ok bool, outcome, msg string, credits float64) JobResult {
	r := JobResult{Job: job, OK: ok, Outcome: outcome, Message: msg, Credits: credits, At: time.Now(), Trigger: "manual"}
	if a != nil {
		r.UID, r.Nickname = a.UID, a.Nickname
	}
	s.js.mu.Lock()
	key := job
	if p, ok := parentJob[job]; ok {
		if _, has := s.js.active[job]; !has {
			key = p
		}
	}
	if rt := s.js.active[key]; rt != nil {
		r.RunID, r.Trigger = rt.rep.ID, rt.rep.Trigger
		rt.rep.Accounts++
		switch {
		case outcome == "skipped":
			rt.rep.Skipped++
		case ok:
			rt.rep.OK++
		default:
			rt.rep.Failed++
		}
		rt.rep.Credits += credits
		if len(rt.rep.Results) < 500 {
			rt.rep.Results = append(rt.rep.Results, r)
		}
	}
	obs := s.js.observer
	s.js.mu.Unlock()
	if obs != nil {
		obs.JobResult(r)
	}
	return r
}

// noteErr 失败结果的便捷写法。
func (s *Scheduler) noteErr(job string, a *auth.Auth, err error) JobResult {
	return s.note(job, a, false, "failed", err.Error(), 0)
}

// ---------------------------------------------------------------------------
// 国际版每日活跃
// ---------------------------------------------------------------------------

func cstDay(t time.Time) string { return t.In(cstZone).Format("2006-01-02") }

// RunIntlDailyNow 对所有国际版账号执行每日活跃（手动 = 不论当天是否已成功都执行）。
func (s *Scheduler) RunIntlDailyNow() {
	s.track(JobIntlDaily, "manual", func() { s.runIntlDaily(context.Background(), true) })
}

func (s *Scheduler) runIntlDaily(ctx context.Context, force bool) {
	first := true
	today := cstDay(time.Now())
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" || !a.IsGlobal() {
			continue
		}
		if !force {
			s.js.mu.Lock()
			done := s.js.intlDone[st.UID] == today
			s.js.mu.Unlock()
			if done {
				continue
			}
		}
		if !first && !sleepCtx(ctx, activityAccountDelay) {
			return
		}
		first = false
		s.intlDailyOne(ctx, a)
	}
}

func (s *Scheduler) intlDailyOne(ctx context.Context, a *auth.Auth) JobResult {
	if a == nil || !a.IsGlobal() {
		return s.note(JobIntlDaily, a, true, "skipped", "每日活跃只适用于国际版账号", 0)
	}
	ex := s.GetExtras()
	res, err := s.cfg.Upstream.RunIntlDaily(ctx, a, ex.IntlDailyModels, ex.IntlDailyWeb, ex.IntlDailyPrompt)
	if err != nil {
		log.Printf("intl-daily %s: %v", logfmt.Label(a.UID, a.Nickname), err)
		return s.noteErr(JobIntlDaily, a, err)
	}
	s.js.mu.Lock()
	if s.js.intlDone == nil {
		s.js.intlDone = map[string]string{}
	}
	s.js.intlDone[a.UID] = cstDay(time.Now())
	s.js.mu.Unlock()
	msg := fmt.Sprintf("桌面通道对话完成（%s）", res.Model)
	if res.Web {
		msg = fmt.Sprintf("每日活跃完成：桌面（%s）+ 网页会话跑完（%d 段输出，%d ms）", res.Model, res.Chunks, res.ElapsedMS)
	}
	log.Printf("intl-daily %s: %s", logfmt.Label(a.UID, a.Nickname), msg)
	return s.note(JobIntlDaily, a, true, "done", msg, 0)
}

// ---------------------------------------------------------------------------
// 开 Buddy 盲盒
// ---------------------------------------------------------------------------

// RunOpenBoxesNow 对所有国内版个人账号用多余能量开 Buddy 盲盒。
func (s *Scheduler) RunOpenBoxesNow() {
	s.track(JobOpenBoxes, "manual", func() {
		first := true
		for _, st := range s.cfg.Pool.List() {
			if st.Disabled {
				continue
			}
			a := s.cfg.Pool.AuthByUID(st.UID)
			if a == nil || a.AccessTokenValue() == "" || a.IsGlobal() || a.IsEnterpriseSession() {
				continue
			}
			if !first {
				time.Sleep(travelAccountDelay)
			}
			first = false
			s.openBoxesOne(a, true)
		}
	})
}

// openBoxesOne 单账号开盲盒；quiet=false 时无可开也记一条「跳过」。
func (s *Scheduler) openBoxesOne(a *auth.Auth, recordEmpty bool) JobResult {
	if a == nil || a.IsGlobal() || a.IsEnterpriseSession() {
		return s.note(JobOpenBoxes, a, true, "skipped", "盲盒只适用于国内版个人账号", 0)
	}
	res, err := s.cfg.Upstream.OpenBuddyBoxes(a)
	if err != nil {
		log.Printf("open-boxes %s: %v", logfmt.Label(a.UID, a.Nickname), err)
		return s.noteErr(JobOpenBoxes, a, err)
	}
	if res.Opened == 0 {
		if recordEmpty {
			return s.note(JobOpenBoxes, a, true, "skipped", "能量不足，没有可开的盲盒", 0)
		}
		return JobResult{Job: JobOpenBoxes, OK: true, Outcome: "skipped"}
	}
	msg := fmt.Sprintf("用能量开 Buddy 盲盒 ×%d %s", res.Opened, res.Detail)
	log.Printf("open-boxes %s: %s", logfmt.Label(a.UID, a.Nickname), msg)
	return s.note(JobOpenBoxes, a, true, "done", strings.TrimSpace(msg), 0)
}

// ---------------------------------------------------------------------------
// 单账号执行（面板账号卡片 / 任务管理行内的「立即执行」）
// ---------------------------------------------------------------------------

// ErrUnknownJob 不支持单账号执行的任务。
var ErrUnknownJob = errors.New("unknown job")

// RunJobFor 对单个账号立即执行一个任务，返回其结果（也写入任务记录）。
func (s *Scheduler) RunJobFor(job, uid string) (JobResult, error) {
	switch job {
	case JobCheckin, JobTravel, JobOpenBoxes, JobActivity, JobKeepalive, JobIntlDaily, JobStreak:
	default:
		return JobResult{}, ErrUnknownJob
	}
	if s.cfg.Pool == nil {
		return JobResult{}, errors.New("账号不存在")
	}
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return JobResult{}, errors.New("账号不存在")
	}
	var out JobResult
	run := func() {
		switch job {
		case JobCheckin:
			out = s.checkinOne(a)
		case JobTravel:
			out = s.travelOneRecorded(a)
		case JobOpenBoxes:
			out = s.openBoxesOne(a, true)
		case JobActivity:
			out = s.activityOne(a)
		case JobKeepalive:
			out = s.keepaliveOne(a, false)
		case JobIntlDaily:
			out = s.intlDailyOne(context.Background(), a)
		case JobStreak:
			out = s.streakOne(a)
		}
	}
	s.track(job, "manual", run)
	return out, nil
}

// ActiveJobs 正在执行中的批次（job → 开始时间），供面板显示「执行中」。
func (s *Scheduler) ActiveJobs() map[string]time.Time {
	out := map[string]time.Time{}
	s.js.mu.Lock()
	defer s.js.mu.Unlock()
	for job, rt := range s.js.active {
		out[job] = rt.rep.Started
	}
	return out
}

// NextRuns 各定时任务下一次触发时刻（总开关关闭 / 任务停用时不出现）。
func (s *Scheduler) NextRuns(now time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	s.schedMu.Lock()
	c, ex := s.cfg, s.extras
	s.schedMu.Unlock()
	if !ex.Enabled {
		return out
	}
	add := func(job string, off bool, hours []int) {
		if off || len(hours) == 0 {
			return
		}
		if t := nextFire(now, hours); !t.IsZero() {
			out[job] = t
		}
	}
	add(JobCheckin, c.CheckinDisabled, c.CheckinHours)
	add(JobTravel, c.TravelDisabled, c.TravelHours)
	add(JobActivity, c.ActivityDisabled, c.ActivityHours)
	add(JobKeepalive, c.KeepaliveDisabled, c.KeepaliveHours)
	add(JobBlackcat, c.BlackcatDisabled, c.BlackcatHours)
	add(JobGrowth, c.GrowthDisabled || c.GrowthHook == nil, c.GrowthHours)
	add(JobIntlDaily, !ex.IntlDailyEnabled, ex.IntlDailyHours)
	return out
}

// RunStreakNow 对全部账号执行连登管家（补签 / 礼包 / 补偿 / 兑换 / 抽奖）。
func (s *Scheduler) RunStreakNow() { s.track(JobStreak, "manual", s.RunStreakBonusNow) }
