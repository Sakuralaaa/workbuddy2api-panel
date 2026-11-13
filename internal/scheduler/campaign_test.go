package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// fakeCampaigns 模拟上游 /portal/activity/* + /v2/report + 成长任务清单。
type fakeCampaigns struct {
	mu       sync.Mutex
	tasks    map[string][]map[string]any // code → 任务定义
	progress map[string]map[string]int   // code → task → 进度
	claimed  map[string]map[string]bool
	claims   []string
	reports  int
}

func newFakeCampaigns() *fakeCampaigns {
	return &fakeCampaigns{
		tasks: map[string][]map[string]any{
			"campaign_starbucks_2026": {
				{"task_code": "ai_chat", "target_count": 3, "reward_credit": 100, "need_view": false},
				{"task_code": "app", "target_count": 1, "reward_credit": 100},
			},
			// 未知新活动：只出现在成长任务清单的跳转链接里
			"campaign_newyear_2027": {
				{"task_code": "chat_twice", "task_name": "对话 2 次", "target_count": 2, "reward_credit": 30},
				{"task_code": "scan_poster", "task_name": "扫码海报", "task_type": "scan_qr", "reward_credit": 10},
			},
		},
		progress: map[string]map[string]int{"campaign_starbucks_2026": {}, "campaign_newyear_2027": {}},
		claimed:  map[string]map[string]bool{"campaign_starbucks_2026": {}, "campaign_newyear_2027": {}},
	}
}

func (f *fakeCampaigns) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := func(data any) {
		raw, _ := json.Marshal(map[string]any{"code": 0, "msg": "OK", "data": data})
		w.Write(raw)
	}
	switch {
	case r.URL.Path == "/v2/activity/growth/tasks":
		ok(map[string]any{"tasks": []map[string]any{{"task_code": "x", "jump_url": "/pages/act?activity_code=campaign_newyear_2027"}}})
	case r.URL.Path == "/v2/report":
		body, _ := io.ReadAll(r.Body)
		var evs []map[string]any
		_ = json.Unmarshal(body, &evs)
		f.reports++
		for _, ev := range evs {
			if ev["eventCode"] == "chat_request_send" {
				// 判据：每条对话事件给所有活动的 chat 任务 +1
				f.progress["campaign_starbucks_2026"]["ai_chat"]++
				f.progress["campaign_newyear_2027"]["chat_twice"]++
			}
		}
		ok(nil)
	case r.URL.Path == "/portal/activity/tasks":
		code := r.URL.Query().Get("activity_code")
		defs, has := f.tasks[code]
		if !has {
			ok(map[string]any{"tasks": []any{}, "progress": []any{}})
			return
		}
		var prog []map[string]any
		for _, d := range defs {
			tc := d["task_code"].(string)
			tgt := 1
			if v, ok := d["target_count"].(int); ok {
				tgt = v
			}
			p := map[string]any{"task_code": tc, "progress": f.progress[code][tc]}
			if f.progress[code][tc] >= tgt {
				p["completed_at"] = "2026-11-14T10:00:00Z"
			}
			if f.claimed[code][tc] {
				p["claimed_at"] = "2026-11-14T10:01:00Z"
			}
			prog = append(prog, p)
		}
		ok(map[string]any{"tasks": defs, "progress": prog})
	case strings.HasPrefix(r.URL.Path, "/portal/activity/tasks/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/portal/activity/tasks/"), "/")
		code := r.URL.Query().Get("activity_code")
		if len(parts) == 2 && parts[1] == "claim" {
			f.claimed[code][parts[0]] = true
			f.claims = append(f.claims, code+"/"+parts[0])
		}
		ok(nil)
	default:
		http.Error(w, "not found", 404)
	}
}

func campaignScheduler(t *testing.T, srv *httptest.Server) (*Scheduler, *pool.Pool) {
	t.Helper()
	oldGap, oldTries := campaignPollGap, campaignPollTries
	campaignPollGap, campaignPollTries = 0, 2
	fastActivity(t)
	t.Cleanup(func() { campaignPollGap, campaignPollTries = oldGap, oldTries })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "甲", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	if err := s.SetCampaignFile(filepath.Join(t.TempDir(), "activities.json")); err != nil {
		t.Fatal(err)
	}
	return s, p
}

// 发现：已知活动 + 从成长任务清单扫到的未知新活动都被列出，新活动带角标；只读不执行。
func TestCampaignDiscoverFindsNewActivity(t *testing.T) {
	f := newFakeCampaigns()
	srv := httptest.NewServer(f)
	defer srv.Close()
	s, _ := campaignScheduler(t, srv)

	s.DiscoverCampaignsNow(context.Background())
	snap := s.CampaignSnapshot()
	if len(snap.Accounts) != 1 || len(snap.Accounts[0].Activities) != 2 {
		t.Fatalf("snapshot=%+v", snap)
	}
	if !snap.Seen["campaign_newyear_2027"].New || snap.NewCount != 2 {
		t.Fatalf("seen=%+v new=%d", snap.Seen, snap.NewCount)
	}
	if f.reports != 0 || len(f.claims) != 0 {
		t.Fatalf("discover must not execute: reports=%d claims=%v", f.reports, f.claims)
	}
	s.MarkCampaignsSeen()
	if s.CampaignSnapshot().NewCount != 0 {
		t.Fatal("mark seen did not clear badge")
	}
	// 持久化：新实例读回同一份快照
	s2 := New(Config{Pool: s.cfg.Pool, Upstream: s.cfg.Upstream})
	if err := s2.SetCampaignFile(s.camp.path); err != nil {
		t.Fatal(err)
	}
	if got := s2.CampaignSnapshot(); len(got.Seen) != 2 || got.NewCount != 0 {
		t.Fatalf("reload=%+v", got)
	}
}

// 执行：chat 类任务上报到达标并领奖（含未知活动），需真人 / 未知类型的任务列为需手动不硬跑。
func TestCampaignRunCompletesGenericTasks(t *testing.T) {
	f := newFakeCampaigns()
	srv := httptest.NewServer(f)
	defer srv.Close()
	s, _ := campaignScheduler(t, srv)
	var got []JobResult
	s.SetObserver(obsFunc(func(r JobResult) { got = append(got, r) }))

	res, err := s.RunJobFor(JobCampaign, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Outcome != "claimed" || res.Credits != 130 {
		t.Fatalf("result=%+v", res)
	}
	if !strings.Contains(res.Message, "2 个需手动") {
		t.Fatalf("manual not reported: %s", res.Message)
	}
	claims := strings.Join(f.claims, ",")
	if !strings.Contains(claims, "campaign_starbucks_2026/ai_chat") || !strings.Contains(claims, "campaign_newyear_2027/chat_twice") {
		t.Fatalf("claims=%v", f.claims)
	}
	if strings.Contains(claims, "app") || strings.Contains(claims, "scan_poster") {
		t.Fatalf("manual task claimed: %v", f.claims)
	}
	if len(got) == 0 || got[len(got)-1].Job != JobCampaign {
		t.Fatalf("no task record: %+v", got)
	}
	// 回读后快照里 chat 任务已领奖
	for _, act := range s.CampaignSnapshot().Accounts[0].Activities {
		for _, tk := range act.Tasks {
			if tk.Kind == upstream.ActivityKindChat && tk.Status() != "claimed" {
				t.Fatalf("%s/%s status=%s", act.Code, tk.Code, tk.Status())
			}
		}
	}
	// 第二轮：都领过了，不再上报
	before := f.reports
	if r2, _ := s.RunJobFor(JobCampaign, "u1"); r2.Credits != 0 || f.reports != before {
		t.Fatalf("second run should be idempotent: %+v reports %d→%d", r2, before, f.reports)
	}
}

// 国际版 / 企业版跳过。
func TestCampaignSkipsIneligible(t *testing.T) {
	f := newFakeCampaigns()
	srv := httptest.NewServer(f)
	defer srv.Close()
	s, p := campaignScheduler(t, srv)
	p.Add(&auth.Auth{UID: "ent", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, EnterpriseID: "e1"})
	res, err := s.RunJobFor(JobCampaign, "ent")
	if err != nil || res.Outcome != "skipped" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestCampaignScheduleSlot(t *testing.T) {
	s := New(Config{CheckinDisabled: true, TravelDisabled: true, ActivityDisabled: true, KeepaliveDisabled: true, BlackcatDisabled: true, GrowthDisabled: true})
	s.SetExtras(Extras{Enabled: true, ActivityTasksEnabled: true, ActivityTasksHours: []int{11}})
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, cstZone)
	at, kinds := s.nextWake(now)
	if len(kinds) != 1 || kinds[0] != taskCampaign || at.In(cstZone).Hour() != 11 {
		t.Fatalf("at=%v kinds=%v", at, kinds)
	}
	if _, ok := s.NextRuns(now)[JobCampaign]; !ok {
		t.Fatal("NextRuns missing campaign")
	}
	s.SetExtras(Extras{Enabled: true})
	if _, kinds := s.nextWake(now); len(kinds) != 0 {
		t.Fatalf("disabled campaign still scheduled: %v", kinds)
	}
}

type obsFunc func(JobResult)

func (o obsFunc) JobResult(r JobResult) { o(r) }
func (o obsFunc) RunDone(RunReport)     {}
