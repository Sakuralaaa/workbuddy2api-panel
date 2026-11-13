// Package panelstore 面板侧需要跨重启保存的数据（panel-v3）：账号备注、每账号任务记录、
// 结构化运行历史。与 state.json / usage.json 同目录落盘（随 state_file 搬移），原子写。
//
// 容量：任务记录全局保留最近 MaxRecords 条（面板按账号筛选），运行历史保留最近 MaxRuns 轮。
package panelstore

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 容量上限。
const (
	MaxRecords = 4000
	MaxRuns    = 300
	MaxNoteLen = 60
)

// FlushInterval 脏数据后台落盘间隔。
var FlushInterval = 3 * time.Second

// Record 一条账号级任务记录。
type Record struct {
	ID       int64     `json:"id"`
	RunID    string    `json:"run_id,omitempty"`
	Trigger  string    `json:"trigger"`
	Job      string    `json:"job"`
	JobName  string    `json:"job_name,omitempty"`
	UID      string    `json:"uid"`
	Nickname string    `json:"nickname,omitempty"`
	OK       bool      `json:"ok"`
	Outcome  string    `json:"outcome"`
	Message  string    `json:"message"`
	Credits  float64   `json:"credits,omitempty"`
	At       time.Time `json:"at"`
}

// Run 一轮批量执行的汇总。
type Run struct {
	ID       string    `json:"id"`
	Job      string    `json:"job"`
	JobName  string    `json:"job_name,omitempty"`
	Trigger  string    `json:"trigger"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Accounts int       `json:"accounts"`
	OK       int       `json:"ok"`
	Failed   int       `json:"failed"`
	Skipped  int       `json:"skipped"`
	Credits  float64   `json:"credits"`
}

type fileData struct {
	Notes   map[string]string `json:"notes"`
	Records []Record          `json:"records"`
	Runs    []Run             `json:"runs"`
	NextID  int64             `json:"next_id"`
}

// Store 并发安全；nil *Store 的方法安全返回空值。
type Store struct {
	path  string
	mu    sync.Mutex
	d     fileData
	dirty bool

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// Open 读取 path（不存在则空表）。path 为空 = 纯内存。
func Open(path string) (*Store, error) {
	s := &Store{path: path, d: fileData{Notes: map[string]string{}}}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.d); err != nil {
		return nil, err
	}
	if s.d.Notes == nil {
		s.d.Notes = map[string]string{}
	}
	return s, nil
}

// Start 启动后台落盘。
func (s *Store) Start() {
	if s == nil || s.path == "" || s.stop != nil {
		return
	}
	s.stop, s.done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(s.done)
		t := time.NewTicker(FlushInterval)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				_ = s.Flush()
			}
		}
	}()
}

// Close 停止后台循环并落盘。
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		if s.stop != nil {
			close(s.stop)
			<-s.done
		}
	})
	return s.Flush()
}

// Flush 有脏数据时原子写盘。
func (s *Store) Flush() error {
	if s == nil || s.path == "" {
		return nil
	}
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	raw, err := json.Marshal(s.d)
	s.dirty = false
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := writeAtomic(s.path, raw); err != nil {
		s.mu.Lock()
		s.dirty = true
		s.mu.Unlock()
		return err
	}
	return nil
}

func writeAtomic(path string, raw []byte) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------------------
// 备注
// ---------------------------------------------------------------------------

// Notes 全部备注（uid → 备注）副本。
func (s *Store) Notes() map[string]string {
	out := map[string]string{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.d.Notes {
		out[k] = v
	}
	return out
}

// Note 单个账号的备注。
func (s *Store) Note(uid string) string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Notes[uid]
}

// SetNote 设置备注（空串 = 删除），立即落盘。
func (s *Store) SetNote(uid, note string) error {
	if s == nil {
		return errors.New("panel store not available")
	}
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return errors.New("uid 不能为空")
	}
	note = strings.TrimSpace(note)
	if r := []rune(note); len(r) > MaxNoteLen {
		note = string(r[:MaxNoteLen])
	}
	s.mu.Lock()
	if note == "" {
		delete(s.d.Notes, uid)
	} else {
		s.d.Notes[uid] = note
	}
	s.dirty = true
	s.mu.Unlock()
	return s.Flush()
}

// MergeNotes 批量写入（旧版本机 localStorage 备注迁移用）：只填补服务端没有的账号，返回写入条数。
func (s *Store) MergeNotes(m map[string]string) int {
	if s == nil {
		return 0
	}
	n := 0
	s.mu.Lock()
	for uid, note := range m {
		uid, note = strings.TrimSpace(uid), strings.TrimSpace(note)
		if uid == "" || note == "" || s.d.Notes[uid] != "" {
			continue
		}
		if r := []rune(note); len(r) > MaxNoteLen {
			note = string(r[:MaxNoteLen])
		}
		s.d.Notes[uid] = note
		n++
	}
	if n > 0 {
		s.dirty = true
	}
	s.mu.Unlock()
	if n > 0 {
		_ = s.Flush()
	}
	return n
}

// ---------------------------------------------------------------------------
// 任务记录 / 运行历史
// ---------------------------------------------------------------------------

// AddRecord 追加一条任务记录（自动编号）。
func (s *Store) AddRecord(r Record) Record {
	if s == nil {
		return r
	}
	if r.At.IsZero() {
		r.At = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.NextID++
	r.ID = s.d.NextID
	s.d.Records = append(s.d.Records, r)
	if over := len(s.d.Records) - MaxRecords; over > 0 {
		s.d.Records = append([]Record(nil), s.d.Records[over:]...)
	}
	s.dirty = true
	return r
}

// Records 最近的记录（新的在前）；uid/job 非空时精确过滤；limit<=0 = 200。
func (s *Store) Records(uid, job string, limit int) []Record {
	out := []Record{}
	if s == nil {
		return out
	}
	if limit <= 0 {
		limit = 200
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.d.Records) - 1; i >= 0 && len(out) < limit; i-- {
		r := s.d.Records[i]
		if uid != "" && r.UID != uid {
			continue
		}
		if job != "" && r.Job != job {
			continue
		}
		out = append(out, r)
	}
	return out
}

// LastByAccount 每个账号每个任务的最近一条记录：uid → job → 记录。
func (s *Store) LastByAccount() map[string]map[string]Record {
	out := map[string]map[string]Record{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.d.Records {
		m := out[r.UID]
		if m == nil {
			m = map[string]Record{}
			out[r.UID] = m
		}
		m[r.Job] = r
	}
	return out
}

// AddRun 追加一轮运行汇总。
func (s *Store) AddRun(r Run) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Runs = append(s.d.Runs, r)
	if over := len(s.d.Runs) - MaxRuns; over > 0 {
		s.d.Runs = append([]Run(nil), s.d.Runs[over:]...)
	}
	s.dirty = true
}

// Runs 最近的运行（新的在前）；limit<=0 = 100。
func (s *Store) Runs(limit int) []Run {
	out := []Run{}
	if s == nil {
		return out
	}
	if limit <= 0 {
		limit = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.d.Runs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.d.Runs[i])
	}
	return out
}

// RunRecords 某一轮的全部账号记录（按时间顺序）。
func (s *Store) RunRecords(runID string) []Record {
	out := []Record{}
	if s == nil || runID == "" {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.d.Records {
		if r.RunID == runID {
			out = append(out, r)
		}
	}
	return out
}

// ClearHistory 清空任务记录与运行历史（备注保留）。
func (s *Store) ClearHistory() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.d.Records, s.d.Runs = nil, nil
	s.dirty = true
	s.mu.Unlock()
	_ = s.Flush()
}
