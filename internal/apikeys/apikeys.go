// Package apikeys 多 API Key 管理与按 Key 的用量统计。
//
// 设计：
//   - 密钥明文只在创建时返回一次；落盘只存 SHA-256 摘要与脱敏前后缀（展示用），
//     与 state.json / usage.json / credit-history.json 一样落在数据目录（随 state_file 搬移），
//     本地文件即持久化层，重启后保留。
//   - 鉴权：Verify 对 token 做 SHA-256 后按摘要查表（摘要不可逆、定长，避免明文比较的计时侧信道）。
//   - 用量：按 key → 自然日（Asia/Shanghai）→ 模型 聚合请求数 / 成功 / 失败 / tokens / 积分；
//     保留最近 RetentionDays 天。写入只改内存并标脏，后台每 FlushInterval 落盘一次，
//     进程退出时 Close 再落一次。
//   - 兼容：config.api_key（旧单密钥）不进本表，由调用方以固定 id LegacyID 计入用量。
package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 固定 id：旧 config.api_key 与面板内部测试请求在用量统计里的归属。
const (
	LegacyID     = "legacy"
	LegacyName   = "旧主密钥（api_key）"
	InternalID   = "panel"
	InternalName = "面板测试"
)

// RetentionDays 用量明细保留天数。
const RetentionDays = 90

// FlushInterval 用量脏数据的后台落盘间隔。
var FlushInterval = 5 * time.Second

// cst 统计按北京时间自然日分桶（与上游每日重置口径一致；中国无夏令时，固定 +8）。
var cst = time.FixedZone("CST", 8*3600)

// Day 返回 t 所属的统计日（CST，2006-01-02）。
func Day(t time.Time) string { return t.In(cst).Format("2006-01-02") }

// Key 一条 API Key 记录（不含明文）。
type Key struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Hash       string    `json:"hash"`    // sha256(明文) hex
	Display    string    `json:"display"` // 脱敏展示：sk-wb-abcd…wxyz
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
}

// Public 对外视图（不含摘要）。
type Public struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Display    string    `json:"display"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
}

func (k *Key) public() Public {
	return Public{ID: k.ID, Name: k.Name, Display: k.Display, Enabled: k.Enabled, CreatedAt: k.CreatedAt, LastUsedAt: k.LastUsedAt}
}

// Bucket 一个 (key, 日, 模型) 单元的累计量。
type Bucket struct {
	Requests         int64   `json:"requests"`
	Success          int64   `json:"success"`
	Failed           int64   `json:"failed"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Credits          float64 `json:"credits"`
}

func (b *Bucket) add(o Bucket) {
	b.Requests += o.Requests
	b.Success += o.Success
	b.Failed += o.Failed
	b.PromptTokens += o.PromptTokens
	b.CompletionTokens += o.CompletionTokens
	b.TotalTokens += o.TotalTokens
	b.Credits += o.Credits
}

// Usage 一次请求的用量（调用方从请求日志事件转换而来）。
type Usage struct {
	KeyID            string
	Time             time.Time
	Model            string
	OK               bool
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	Credits          float64
}

type fileData struct {
	Keys []*Key `json:"keys"`
	// Imported 已从配置 api_key 迁入过的密钥摘要：删掉后重启不会再被迁回来。
	Imported []string                                 `json:"imported,omitempty"`
	Usage    map[string]map[string]map[string]*Bucket `json:"usage"` // key → day → model
}

// Store 密钥表 + 用量统计。零值不可用，用 Open 构建；nil *Store 的读方法安全返回空。
type Store struct {
	path string

	mu       sync.Mutex
	keys     []*Key
	byHash   map[string]*Key
	imported map[string]bool
	usage    map[string]map[string]map[string]*Bucket
	dirty    bool

	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// Open 读取（不存在则新建空表）path 指向的 JSON 文件。path 为空 = 纯内存（测试用）。
func Open(path string) (*Store, error) {
	s := &Store{path: path, byHash: map[string]*Key{}, imported: map[string]bool{}, usage: map[string]map[string]map[string]*Bucket{}}
	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			var fd fileData
			if err := json.Unmarshal(raw, &fd); err != nil {
				return nil, fmt.Errorf("parse %s: %w", path, err)
			}
			for _, k := range fd.Keys {
				if k == nil || k.ID == "" || k.Hash == "" {
					continue
				}
				s.keys = append(s.keys, k)
				s.byHash[k.Hash] = k
			}
			if fd.Usage != nil {
				s.usage = fd.Usage
			}
			for _, h := range fd.Imported {
				s.imported[h] = true
			}
		case errors.Is(err, os.ErrNotExist):
		default:
			return nil, err
		}
	}
	return s, nil
}

// Start 启动后台落盘循环（幂等）。
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

// Close 停止后台循环并最后落盘一次。
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
	s.pruneLocked(time.Now())
	raw, err := json.MarshalIndent(fileData{Keys: s.keys, Usage: s.usage, Imported: s.importedListLocked()}, "", " ")
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

// saveNowLocked 密钥表变更立即落盘（持锁调用；写盘失败返回错误，内存状态保留）。
func (s *Store) saveNowLocked() error {
	s.dirty = true
	if s.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(fileData{Keys: s.keys, Usage: s.usage, Imported: s.importedListLocked()}, "", " ")
	if err != nil {
		return err
	}
	if err := writeAtomic(s.path, raw); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

func (s *Store) pruneLocked(now time.Time) {
	cut := Day(now.AddDate(0, 0, -RetentionDays))
	for _, days := range s.usage {
		for d := range days {
			if d < cut {
				delete(days, d)
			}
		}
	}
}

func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func displayOf(token string) string {
	if len(token) <= 12 {
		return token[:min(4, len(token))] + "…"
	}
	return token[:9] + "…" + token[len(token)-4:]
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func randID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "k_" + hex.EncodeToString(b)
}

// ErrNotFound 指定 id 不存在。
var ErrNotFound = errors.New("api key not found")

func cleanName(name string) string {
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > 40 {
		name = string(r[:40])
	}
	return name
}

// Create 新建一个启用中的密钥，返回其视图与明文（明文仅此一次）。
func (s *Store) Create(name string) (Public, string, error) {
	if s == nil {
		return Public{}, "", errors.New("api key store not available")
	}
	name = cleanName(name)
	token := "sk-wb-" + randToken(24)
	k := &Key{ID: randID(), Name: name, Hash: hashOf(token), Display: displayOf(token), Enabled: true, CreatedAt: time.Now().UTC()}
	if k.Name == "" {
		k.Name = "密钥 " + k.ID[2:6]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = append(s.keys, k)
	s.byHash[k.Hash] = k
	if err := s.saveNowLocked(); err != nil {
		return Public{}, "", err
	}
	return k.public(), token, nil
}

func (s *Store) importedListLocked() []string {
	out := make([]string, 0, len(s.imported))
	for h := range s.imported {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// ImportLegacy 把配置里的 api_key（含环境变量 WB2A_API_KEY）迁成密钥表里的一条普通密钥：
// 之后可在「密钥管理」里改名、停用、删除。同一个值只迁一次——删除后重启不会再出现。
// 旧的 legacy 用量并入新密钥，用量页不再出现「旧主密钥」。返回是否新建。
func (s *Store) ImportLegacy(token, name string) (bool, error) {
	if s == nil || strings.TrimSpace(token) == "" {
		return false, nil
	}
	h := hashOf(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.imported[h] {
		return false, nil
	}
	s.imported[h] = true
	if s.byHash[h] != nil {
		return false, s.saveNowLocked()
	}
	name = cleanName(name)
	if name == "" {
		name = "主密钥"
	}
	k := &Key{ID: randID(), Name: name, Hash: h, Display: displayOf(token), Enabled: true, CreatedAt: time.Now().UTC()}
	s.keys = append(s.keys, k)
	s.byHash[h] = k
	if old := s.usage[LegacyID]; old != nil {
		s.usage[k.ID] = old
		delete(s.usage, LegacyID)
	}
	return true, s.saveNowLocked()
}

func (s *Store) findLocked(id string) (int, *Key) {
	for i, k := range s.keys {
		if k.ID == id {
			return i, k
		}
	}
	return -1, nil
}

// Rename 改名。
func (s *Store) Rename(id, name string) error {
	if s == nil {
		return ErrNotFound
	}
	name = cleanName(name)
	if name == "" {
		return errors.New("名称不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, k := s.findLocked(id)
	if k == nil {
		return ErrNotFound
	}
	k.Name = name
	return s.saveNowLocked()
}

// SetEnabled 启用 / 停用。
func (s *Store) SetEnabled(id string, on bool) error {
	if s == nil {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, k := s.findLocked(id)
	if k == nil {
		return ErrNotFound
	}
	k.Enabled = on
	return s.saveNowLocked()
}

// Delete 删除密钥（其历史用量保留，供用量页对账；名称在用量里以 id 展示）。
func (s *Store) Delete(id string) error {
	if s == nil {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i, k := s.findLocked(id)
	if k == nil {
		return ErrNotFound
	}
	s.keys = append(s.keys[:i], s.keys[i+1:]...)
	delete(s.byHash, k.Hash)
	return s.saveNowLocked()
}

// List 全部密钥（创建时间升序）。
func (s *Store) List() []Public {
	out := []Public{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		out = append(out, k.public())
	}
	return out
}

// Count 已建密钥数（含停用）：>0 即开启多密钥鉴权。
func (s *Store) Count() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.keys)
}

// Verify 校验 token：命中且启用返回其视图。停用的密钥返回 false。
func (s *Store) Verify(token string) (Public, bool) {
	if s == nil || token == "" {
		return Public{}, false
	}
	h := hashOf(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.byHash[h]
	if k == nil || !k.Enabled {
		return Public{}, false
	}
	return k.public(), true
}

// Name 返回 id 对应的名称（含两个固定 id）；未知 id 原样返回。
func (s *Store) Name(id string) string {
	switch id {
	case LegacyID:
		return LegacyName
	case InternalID:
		return InternalName
	}
	if s == nil {
		return id
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, k := s.findLocked(id); k != nil {
		return k.Name
	}
	return id
}

// Record 计入一次请求的用量。
func (s *Store) Record(u Usage) {
	if s == nil || u.KeyID == "" {
		return
	}
	if u.Time.IsZero() {
		u.Time = time.Now()
	}
	model := u.Model
	if model == "" {
		model = "(未知)"
	}
	b := Bucket{Requests: 1, PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, TotalTokens: u.TotalTokens, Credits: u.Credits}
	if b.TotalTokens == 0 {
		b.TotalTokens = b.PromptTokens + b.CompletionTokens
	}
	if u.OK {
		b.Success = 1
	} else {
		b.Failed = 1
	}
	day := Day(u.Time)
	s.mu.Lock()
	defer s.mu.Unlock()
	days := s.usage[u.KeyID]
	if days == nil {
		days = map[string]map[string]*Bucket{}
		s.usage[u.KeyID] = days
	}
	models := days[day]
	if models == nil {
		models = map[string]*Bucket{}
		days[day] = models
	}
	cur := models[model]
	if cur == nil {
		cur = &Bucket{}
		models[model] = cur
	}
	cur.add(b)
	if _, k := s.findLocked(u.KeyID); k != nil && u.Time.After(k.LastUsedAt) {
		k.LastUsedAt = u.Time.UTC()
	}
	s.dirty = true
}

// KeyReport 单个密钥在区间内的统计。
type KeyReport struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Total   Bucket            `json:"total"`
	ByDay   map[string]Bucket `json:"by_day"`
	ByModel map[string]Bucket `json:"by_model"`
	Days    []DayModelRow     `json:"rows"`
}

// DayModelRow 一行明细（日 × 模型）。
type DayModelRow struct {
	Day   string `json:"day"`
	Model string `json:"model"`
	Bucket
}

// Report 区间统计：[fromDay, toDay]（含端点，空 = 不设界），按密钥汇总。
type Report struct {
	From  string      `json:"from"`
	To    string      `json:"to"`
	Days  []string    `json:"days"`
	Total Bucket      `json:"total"`
	Keys  []KeyReport `json:"keys"`
}

// Report 生成区间统计。只返回区间内有数据的密钥，外加当前所有密钥（零值行），
// 让「新建未使用」的密钥也出现在表里。
func (s *Store) Report(fromDay, toDay string) Report {
	rep := Report{From: fromDay, To: toDay, Keys: []KeyReport{}, Days: []string{}}
	if s == nil {
		return rep
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	daySet := map[string]bool{}
	ids := map[string]bool{}
	for _, k := range s.keys {
		ids[k.ID] = true
	}
	for id := range s.usage {
		ids[id] = true
	}
	for id := range ids {
		kr := KeyReport{ID: id, ByDay: map[string]Bucket{}, ByModel: map[string]Bucket{}, Days: []DayModelRow{}}
		switch id {
		case LegacyID:
			kr.Name = LegacyName
		case InternalID:
			kr.Name = InternalName
		default:
			if _, k := s.findLocked(id); k != nil {
				kr.Name = k.Name
			} else {
				kr.Name = id + "（已删除）"
			}
		}
		for day, models := range s.usage[id] {
			if fromDay != "" && day < fromDay || toDay != "" && day > toDay {
				continue
			}
			daySet[day] = true
			for model, b := range models {
				kr.Total.add(*b)
				d := kr.ByDay[day]
				d.add(*b)
				kr.ByDay[day] = d
				m := kr.ByModel[model]
				m.add(*b)
				kr.ByModel[model] = m
				kr.Days = append(kr.Days, DayModelRow{Day: day, Model: model, Bucket: *b})
			}
		}
		sort.Slice(kr.Days, func(i, j int) bool {
			if kr.Days[i].Day != kr.Days[j].Day {
				return kr.Days[i].Day > kr.Days[j].Day
			}
			return kr.Days[i].Requests > kr.Days[j].Requests
		})
		_, live := s.findLocked(id)
		if kr.Total.Requests == 0 && live == nil {
			continue // 已删除且区间内无数据
		}
		rep.Total.add(kr.Total)
		rep.Keys = append(rep.Keys, kr)
	}
	sort.Slice(rep.Keys, func(i, j int) bool {
		if rep.Keys[i].Total.Requests != rep.Keys[j].Total.Requests {
			return rep.Keys[i].Total.Requests > rep.Keys[j].Total.Requests
		}
		return rep.Keys[i].Name < rep.Keys[j].Name
	})
	for d := range daySet {
		rep.Days = append(rep.Days, d)
	}
	sort.Strings(rep.Days)
	return rep
}
