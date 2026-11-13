// v3.go panel-v3 新增接口：登录封面鉴权探测、多 API Key、模型启停与测试、账号备注、
// 任务记录 / 运行历史、单账号与「全部账号执行」任务入口、调度状态。
package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panelstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
)

// ModelTestResult 测试模型结果。
type ModelTestResult struct {
	OK        bool   `json:"ok"`
	Model     string `json:"model"`
	Status    int    `json:"status"`
	LatencyMs int64  `json:"latency_ms"`
	Reply     string `json:"reply,omitempty"`
	Error     string `json:"error,omitempty"`
	Account   string `json:"account,omitempty"`
}

func (p *Panel) routesV3() {
	// 登录封面：无鉴权探测是否需要密钥（不泄露任何数据）；check 带鉴权，用于校验输入的管理密钥。
	p.mux.HandleFunc("GET /panel/api/auth/info", p.authInfo)
	p.mux.HandleFunc("GET /panel/api/auth/check", p.withAuth(p.authCheck))

	p.mux.HandleFunc("GET /panel/api/keys", p.withAuth(p.keysList))
	p.mux.HandleFunc("POST /panel/api/keys", p.withAuth(p.keysCreate))
	p.mux.HandleFunc("POST /panel/api/keys/{id}/rename", p.withAuth(p.keysRename))
	p.mux.HandleFunc("POST /panel/api/keys/{id}/enabled", p.withAuth(p.keysEnabled))
	p.mux.HandleFunc("POST /panel/api/keys/{id}/delete", p.withAuth(p.keysDelete))
	p.mux.HandleFunc("GET /panel/api/keys/usage", p.withAuth(p.keysUsage))

	p.mux.HandleFunc("POST /panel/api/models/toggle", p.withAuth(p.modelsToggle))
	p.mux.HandleFunc("POST /panel/api/models/test", p.withAuth(p.modelsTest))

	p.mux.HandleFunc("GET /panel/api/notes", p.withAuth(p.notesList))
	p.mux.HandleFunc("POST /panel/api/notes/import", p.withAuth(p.notesImport))
	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/note", p.withAuth(p.accountNote))

	p.mux.HandleFunc("GET /panel/api/task_records", p.withAuth(p.taskRecords))
	p.mux.HandleFunc("GET /panel/api/runs", p.withAuth(p.runsList))
	p.mux.HandleFunc("GET /panel/api/runs/{id}", p.withAuth(p.runDetail))
	p.mux.HandleFunc("POST /panel/api/runs/clear", p.withAuth(p.runsClear))

	p.mux.HandleFunc("POST /panel/api/accounts/{uid}/run", p.withAuth(p.accountRunJob))
	p.mux.HandleFunc("POST /panel/api/jobs/{job}/run_all", p.withAuth(p.jobRunAll))
	p.mux.HandleFunc("GET /panel/api/schedule/status", p.withAuth(p.scheduleStatus))

	p.mux.HandleFunc("GET /panel/api/activities", p.withAuth(p.activitiesList))
	p.mux.HandleFunc("POST /panel/api/activities/discover", p.withAuth(p.activitiesDiscover))
	p.mux.HandleFunc("POST /panel/api/activities/seen", p.withAuth(p.activitiesSeen))
}

// storageLabel 状态栏「服务正常 · xxx」里的存储说明（反映实际持久化层）。
func (p *Panel) storageLabel() string {
	if p.cfg.RedisMode == "upstash" {
		return "本地文件 + Upstash Redis"
	}
	return "本地内存"
}

func (p *Panel) authInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_required": p.apiKey() != "",
		"admin_key":     p.cfg.Live != nil && p.cfg.Live.Load().AdminKey != "" || p.cfg.AdminKey != "",
	})
}

func (p *Panel) authCheck(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "storage": p.storageLabel()})
}

// ---------------------------------------------------------------------------
// API Keys
// ---------------------------------------------------------------------------

func (p *Panel) keysList(w http.ResponseWriter, r *http.Request) {
	legacy := ""
	if p.cfg.Live != nil {
		legacy = p.cfg.Live.Load().APIKey
	} else {
		legacy = p.cfg.APIKey
	}
	masked := ""
	if legacy != "" {
		if len(legacy) > 10 {
			masked = legacy[:6] + "…" + legacy[len(legacy)-4:]
		} else {
			masked = "已设置"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"keys":              p.cfg.Keys.List(),
		"legacy_configured": legacy != "",
		"legacy_display":    masked,
		"auth_enabled":      legacy != "" || p.cfg.Keys.Count() > 0,
		"available":         p.cfg.Keys != nil,
	})
}

func decodeBody(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v)
}

func (p *Panel) keysCreate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Keys == nil {
		writeErr(w, http.StatusNotImplemented, "api key store not available")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	_ = decodeBody(r, &body)
	k, secret, err := p.cfg.Keys.Create(body.Name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	log.Printf("panel: 新建 API Key %s（%s）", k.ID, k.Name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": k, "secret": secret})
}

func (p *Panel) keysRename(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := p.cfg.Keys.Rename(r.PathValue("id"), body.Name); err != nil {
		writeErr(w, keyErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func keyErrStatus(err error) int {
	if err == apikeys.ErrNotFound {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func (p *Panel) keysEnabled(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := p.cfg.Keys.SetEnabled(r.PathValue("id"), body.Enabled); err != nil {
		writeErr(w, keyErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) keysDelete(w http.ResponseWriter, r *http.Request) {
	if err := p.cfg.Keys.Delete(r.PathValue("id")); err != nil {
		writeErr(w, keyErrStatus(err), err.Error())
		return
	}
	log.Printf("panel: 删除 API Key %s", r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keysUsage ?days=N（默认 7，含今天）或 from/to=YYYY-MM-DD。
func (p *Panel) keysUsage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")
	if from == "" && to == "" {
		days := 7
		if n, err := strconv.Atoi(q.Get("days")); err == nil && n > 0 && n <= apikeys.RetentionDays {
			days = n
		}
		now := time.Now()
		to = apikeys.Day(now)
		from = apikeys.Day(now.AddDate(0, 0, -(days - 1)))
	}
	writeJSON(w, http.StatusOK, p.cfg.Keys.Report(from, to))
}

// ---------------------------------------------------------------------------
// 模型启停 / 测试
// ---------------------------------------------------------------------------

func (p *Panel) disabledModels() map[string]struct{} {
	if p.cfg.Live == nil {
		return nil
	}
	return p.cfg.Live.Load().DisabledModels
}

func (p *Panel) modelsToggle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string   `json:"id"`
		IDs     []string `json:"ids"`
		Enabled bool     `json:"enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	ids := body.IDs
	if body.ID != "" {
		ids = append(ids, body.ID)
	}
	if len(ids) == 0 {
		writeErr(w, http.StatusBadRequest, "id required")
		return
	}
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config save not available")
		return
	}
	set := map[string]struct{}{}
	for k := range p.disabledModels() {
		set[k] = struct{}{}
	}
	for _, id := range ids {
		norm := livecfg.NormalizeHiddenModels([]string{id})
		for k := range norm {
			if body.Enabled {
				delete(set, k)
			} else {
				set[k] = struct{}{}
			}
		}
	}
	list := make([]string, 0, len(set))
	for k := range set {
		list = append(list, k)
	}
	sort.Strings(list)
	raw, _ := json.Marshal(map[string]any{"models": map[string]any{"disabled": list}})
	if _, err := p.cfg.SaveConfig(raw); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "disabled": list})
}

func (p *Panel) modelsTest(w http.ResponseWriter, r *http.Request) {
	if p.cfg.TestModel == nil {
		writeErr(w, http.StatusNotImplemented, "model test not available")
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil || strings.TrimSpace(body.Model) == "" {
		writeErr(w, http.StatusBadRequest, "model required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, p.cfg.TestModel(ctx, strings.TrimSpace(body.Model)))
}

// ---------------------------------------------------------------------------
// 备注
// ---------------------------------------------------------------------------

func (p *Panel) notesList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"notes": p.cfg.Store.Notes(), "available": p.cfg.Store != nil})
}

func (p *Panel) accountNote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Note string `json:"note"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := p.cfg.Store.SetNote(r.PathValue("uid"), body.Note); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "note": p.cfg.Store.Note(r.PathValue("uid"))})
}

// notesImport 旧版本机 localStorage 备注一次性迁移：键可以是完整 uid 或 8 位前缀
// （旧版按前 8 位存），前缀按池内账号唯一匹配到完整 uid，匹配不到的丢弃。
func (p *Panel) notesImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Notes map[string]string `json:"notes"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	full := map[string]string{}
	var uids []string
	if p.cfg.Pool != nil {
		for _, s := range p.cfg.Pool.List() {
			uids = append(uids, s.UID)
		}
	}
	for k, v := range body.Notes {
		var hit []string
		for _, u := range uids {
			if u == k || strings.HasPrefix(u, k) {
				hit = append(hit, u)
			}
		}
		if len(hit) == 1 {
			full[hit[0]] = v
		}
	}
	n := p.cfg.Store.MergeNotes(full)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": n, "notes": p.cfg.Store.Notes()})
}

// ---------------------------------------------------------------------------
// 任务记录 / 运行历史
// ---------------------------------------------------------------------------

func (p *Panel) taskRecords(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit > 1000 {
		limit = 1000
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"records": p.cfg.Store.Records(q.Get("uid"), q.Get("job"), limit),
		"last":    p.cfg.Store.LastByAccount(),
		"jobs":    scheduler.JobNames,
	})
}

func (p *Panel) runsList(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	active := map[string]time.Time{}
	if p.cfg.Scheduler != nil {
		active = p.cfg.Scheduler.ActiveJobs()
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": p.cfg.Store.Runs(limit), "active": active, "jobs": scheduler.JobNames})
}

func (p *Panel) runDetail(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"records": p.cfg.Store.RunRecords(r.PathValue("id"))})
}

func (p *Panel) runsClear(w http.ResponseWriter, r *http.Request) {
	p.cfg.Store.ClearHistory()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 任务执行
// ---------------------------------------------------------------------------

func (p *Panel) accountRunJob(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	var body struct {
		Job string `json:"job"`
	}
	if err := decodeBody(r, &body); err != nil || body.Job == "" {
		writeErr(w, http.StatusBadRequest, "job required")
		return
	}
	uid := r.PathValue("uid")
	if !p.tryLockAccount(uid) {
		writeErr(w, http.StatusConflict, "该账号有任务动作正在执行中，请等本轮结束后再试")
		return
	}
	defer p.unlockAccount(uid)
	res, err := p.cfg.Scheduler.RunJobFor(body.Job, uid)
	if err != nil {
		status := http.StatusBadRequest
		if err != scheduler.ErrUnknownJob {
			status = http.StatusNotFound
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": res.OK, "result": res})
}

// bulkJobs 「全部账号执行」可触发的任务（异步）。
func (p *Panel) bulkJobs() map[string]func() {
	s := p.cfg.Scheduler
	if s == nil {
		return nil
	}
	return map[string]func(){
		scheduler.JobCheckin:   s.RunCheckinNow,
		scheduler.JobStreak:    s.RunStreakNow,
		scheduler.JobTravel:    s.RunTravelNow,
		scheduler.JobOpenBoxes: s.RunOpenBoxesNow,
		scheduler.JobActivity:  s.RunActivityNow,
		scheduler.JobKeepalive: s.RunKeepaliveNow,
		scheduler.JobBlackcat:  s.RunBlackcatNow,
		scheduler.JobIntlDaily: s.RunIntlDailyNow,
		scheduler.JobCampaign:  s.RunCampaignNow,
		scheduler.JobTaskSweep: func() { s.RunTaskSweepNow() },
		scheduler.JobBalance:   func() { s.RunBalanceRefreshNow(); p.syncNicknames() },
	}
}

func (p *Panel) jobRunAll(w http.ResponseWriter, r *http.Request) {
	job := r.PathValue("job")
	fn := p.bulkJobs()[job]
	if job == scheduler.JobGrowth {
		fn = func() { p.RunGrowthQueueOnce() }
	}
	if fn == nil {
		writeErr(w, http.StatusBadRequest, "unknown job: "+job)
		return
	}
	if active := p.cfg.Scheduler.ActiveJobs(); !active[job].IsZero() {
		writeErr(w, http.StatusConflict, scheduler.JobNames[job]+"正在执行中")
		return
	}
	go fn()
	log.Printf("panel: 全部账号执行 %s 已触发", scheduler.JobNames[job])
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true, "job": job})
}

func (p *Panel) scheduleStatus(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	s := p.cfg.Scheduler
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": s.Enabled(),
		"extras":  s.GetExtras(),
		"active":  s.ActiveJobs(),
		"next":    s.NextRuns(time.Now()),
		"jobs":    scheduler.JobNames,
		"runs":    p.cfg.Store.Runs(20),
	})
}

// ---------------------------------------------------------------------------
// 面板手动单账号操作的留痕（签到 / 成长任务等走面板自有实现的接口）
// ---------------------------------------------------------------------------

type captureWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (c *captureWriter) WriteHeader(code int) { c.status = code; c.ResponseWriter.WriteHeader(code) }
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.buf.Len() < 8<<10 {
		c.buf.Write(b)
	}
	return c.ResponseWriter.Write(b)
}

// recorded 包装一个账号级面板接口：执行后按响应写一条任务记录（job / 说明由 describe 生成）。
func (p *Panel) recorded(job string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p.cfg.Store == nil {
			next(w, r)
			return
		}
		cw := &captureWriter{ResponseWriter: w, status: http.StatusOK}
		next(cw, r)
		uid := r.PathValue("uid")
		nick := ""
		if a := p.cfg.Pool.AuthByUID(uid); a != nil {
			nick = a.Nickname
		}
		var resp map[string]any
		_ = json.Unmarshal(cw.buf.Bytes(), &resp)
		ok := cw.status < 300
		msg := describeResp(job, resp, cw.status)
		outcome := "done"
		if !ok {
			outcome = "failed"
		}
		if cw.status == http.StatusConflict {
			outcome = "skipped"
		}
		p.cfg.Store.AddRecord(panelstore.Record{
			Trigger: "manual", Job: job, JobName: scheduler.JobNames[job], UID: uid, Nickname: nick,
			OK: ok, Outcome: outcome, Message: msg, At: time.Now(),
		})
	}
}

func describeResp(job string, resp map[string]any, status int) string {
	if e, ok := resp["error"]; ok {
		return fmt.Sprint(e)
	}
	for _, k := range []string{"message", "summary", "checkin_message", "detail"} {
		if v, ok := resp[k].(string); ok && v != "" {
			return v
		}
	}
	switch job {
	case scheduler.JobCheckin:
		if v, ok := resp["credits"]; ok {
			return fmt.Sprintf("签到完成，余额 %v", v)
		}
	case scheduler.JobBalance:
		if v, ok := resp["credits"]; ok {
			return fmt.Sprintf("余额 %v", v)
		}
	case scheduler.JobCompensate:
		if v, ok := resp["credit"]; ok {
			return fmt.Sprintf("领取补偿 +%v", v)
		}
	}
	if code, ok := resp["task_code"].(string); ok {
		return "任务 " + code + " 已执行"
	}
	if status < 300 {
		return "已执行"
	}
	return fmt.Sprintf("HTTP %d", status)
}

// storeObserver 把调度器留痕写入 panelstore。
type storeObserver struct{ s *panelstore.Store }

// NewStoreObserver 供 main 把调度器的执行留痕接到面板存储。
func NewStoreObserver(s *panelstore.Store) scheduler.Observer { return storeObserver{s} }

func (o storeObserver) JobResult(r scheduler.JobResult) {
	o.s.AddRecord(panelstore.Record{
		RunID: r.RunID, Trigger: r.Trigger, Job: r.Job, JobName: scheduler.JobNames[r.Job],
		UID: r.UID, Nickname: r.Nickname, OK: r.OK, Outcome: r.Outcome, Message: r.Message,
		Credits: r.Credits, At: r.At,
	})
}

func (o storeObserver) RunDone(r scheduler.RunReport) {
	o.s.AddRun(panelstore.Run{
		ID: r.ID, Job: r.Job, JobName: scheduler.JobNames[r.Job], Trigger: r.Trigger,
		Started: r.Started, Finished: r.Finished, Accounts: r.Accounts,
		OK: r.OK, Failed: r.Failed, Skipped: r.Skipped, Credits: r.Credits,
	})
}

// ---------------------------------------------------------------------------
// 限时活动（星巴克 / 记者节 等运营活动）
// ---------------------------------------------------------------------------

func (p *Panel) activitiesPayload() map[string]any {
	s := p.cfg.Scheduler
	snap := s.CampaignSnapshot()
	ex := s.GetExtras()
	return map[string]any{
		"accounts":  snap.Accounts,
		"seen":      snap.Seen,
		"new_count": snap.NewCount,
		"last_scan": snap.LastScan,
		"enabled":   ex.Enabled && ex.ActivityTasksEnabled,
		"hours":     ex.ActivityTasksHours,
		"running":   !s.ActiveJobs()[scheduler.JobCampaign].IsZero(),
	}
}

func (p *Panel) activitiesList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	writeJSON(w, http.StatusOK, p.activitiesPayload())
}

// activitiesDiscover 同步发现：逐账号读活动列表（只读，不执行），返回最新快照。
func (p *Panel) activitiesDiscover(w http.ResponseWriter, r *http.Request) {
	s := p.cfg.Scheduler
	if s == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	if !s.ActiveJobs()[scheduler.JobCampaign].IsZero() {
		writeErr(w, http.StatusConflict, "限时活动正在执行中")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	s.DiscoverCampaignsNow(ctx)
	writeJSON(w, http.StatusOK, p.activitiesPayload())
}

func (p *Panel) activitiesSeen(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Scheduler == nil {
		writeErr(w, http.StatusNotImplemented, "scheduler not available")
		return
	}
	p.cfg.Scheduler.MarkCampaignsSeen()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
