package scheduler

import (
	"errors"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

type memObs struct {
	results []JobResult
	runs    []RunReport
}

func (m *memObs) JobResult(r JobResult) { m.results = append(m.results, r) }
func (m *memObs) RunDone(r RunReport)   { m.runs = append(m.runs, r) }

// 调度总开关：关闭后 nextWake 不再排任何时点；重新打开恢复；国际版每日活跃按 extras 进出排程。
func TestMasterSwitchAndIntlDailySlot(t *testing.T) {
	s := New(Config{})
	now := time.Date(2026, 11, 14, 9, 30, 0, 0, time.Local)
	if at, kinds := s.nextWake(now); at.IsZero() || len(kinds) == 0 {
		t.Fatal("default schedule should have a slot")
	}
	s.SetExtras(Extras{Enabled: false})
	if at, _ := s.nextWake(now); !at.IsZero() {
		t.Fatalf("master off still schedules %v", at)
	}
	if len(s.NextRuns(now)) != 0 {
		t.Fatal("NextRuns should be empty when master off")
	}
	s.SetExtras(Extras{Enabled: true, IntlDailyEnabled: true, IntlDailyHours: []int{9}})
	// 9:30 之后最近的 intl_daily 是次日 9 点；签到默认 [9,21] → 21 点先到，不含 intl
	at, kinds := s.nextWake(now)
	for _, k := range kinds {
		if k == taskIntlDaily {
			t.Fatalf("intl daily should not fire at %v", at)
		}
	}
	if _, ok := s.NextRuns(now)[JobIntlDaily]; !ok {
		t.Fatal("intl daily missing from NextRuns")
	}
	s.SetExtras(Extras{Enabled: true, IntlDailyEnabled: true, IntlDailyHours: []int{10}})
	at, kinds = s.nextWake(now)
	found := false
	for _, k := range kinds {
		found = found || k == taskIntlDaily
	}
	if !found || at.Hour() != 10 {
		t.Fatalf("intl daily slot at=%v kinds=%v", at, kinds)
	}
}

// 留痕：一轮批量执行汇总账号结果（成功 / 失败 / 跳过 / 积分），子步骤（连登 → 签到批次）归入父批次，
// 嵌套 track 不重复开批次，批次外的单条结果标为 manual。
func TestTrackAndNote(t *testing.T) {
	s := New(Config{})
	obs := &memObs{}
	s.SetObserver(obs)
	a := &auth.Auth{UID: "u1", Nickname: "甲"}
	s.track(JobCheckin, "schedule", func() {
		s.note(JobCheckin, a, true, "done", "签到成功", 0)
		s.track(JobCheckin, "manual", func() { s.noteErr(JobCheckin, a, errors.New("boom")) })
		s.note(JobStreak, a, true, "skipped", "无", 0)
		s.note(JobTravel, a, true, "claimed", "领奖", 30)
	})
	if len(obs.runs) != 1 {
		t.Fatalf("runs=%d", len(obs.runs))
	}
	r := obs.runs[0]
	if r.Trigger != "schedule" || r.Accounts != 3 || r.OK != 1 || r.Failed != 1 || r.Skipped != 1 {
		t.Fatalf("run=%+v", r)
	}
	if len(obs.results) != 4 || obs.results[2].RunID != r.ID || obs.results[3].RunID != "" || obs.results[3].Trigger != "manual" {
		t.Fatalf("results=%+v", obs.results)
	}
	if len(s.ActiveJobs()) != 0 {
		t.Fatal("active not cleared")
	}
}

func TestRunJobForUnknown(t *testing.T) {
	s := New(Config{})
	if _, err := s.RunJobFor("nope", "x"); err == nil {
		t.Fatal("expected error")
	}
}
