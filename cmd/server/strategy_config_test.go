package main

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegionStrategyConfigCompatibility(t *testing.T) {
	for _, raw := range []string{`{}`, `{"pool":{}}`, `{"pool":{"strategy":{"domestic":""}}}`} {
		c, err := ParseConfig([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if c.Pool.Strategy.Domestic != pool.WeightedRandom || c.Pool.Strategy.International != pool.WeightedRandom {
			t.Fatalf("legacy defaults: %+v", c.Pool.Strategy)
		}
	}
	c, err := ParseConfig([]byte(`{"pool":{"strategy":{"domestic":" ROUND_ROBIN ","international":"sequential_exhaust"},"account_order":{"international":["b","a"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.Strategy.Domestic != pool.RoundRobin || c.Pool.Strategy.International != pool.SequentialExhaust || c.Pool.AccountOrder.International[0] != "b" {
		t.Fatalf("config=%+v", c.Pool)
	}
	for _, raw := range []string{`{"pool":{"strategy":{"domestic":"bad"}}}`, `{"pool":{"strategy":{"international":"bad"}}}`, `{"pool":{"strategy":"round_robin"}}`} {
		_, err := ParseConfig([]byte(raw))
		if err == nil || !strings.Contains(err.Error(), "strategy") {
			t.Fatalf("invalid config err=%v", err)
		}
	}
}

func TestSaveRegionSchedulingHotAppliesAndRejectsInvalid(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "config.json")
	original := []byte(`{"pool":{"strategy":{"international":"sequential_exhaust"}},"custom":"keep"}`)
	if err := os.WriteFile(fp, original, 0600); err != nil {
		t.Fatal(err)
	}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Add(&auth.Auth{UID: "b"})
	live := livecfg.New(livecfg.Snapshot{})
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{})
	raw := []byte(`{"pool":{"strategy":{"domestic":"round_robin"},"account_order":{"domestic":["b","a"]}}}`)
	restart, err := saveConfig(raw, fp, live, p, up, sch)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range restart {
		if strings.HasPrefix(key, "pool.") {
			t.Fatalf("strategy requires restart: %s", key)
		}
	}
	for _, uid := range []string{"b", "a", "b"} {
		if a := p.PickExcludingForRealm(nil, "m", "cn"); a == nil || a.UID != uid {
			t.Fatalf("hot pick=%v want %s", a, uid)
		}
	}
	saved, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfig(saved)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pool.Strategy.International != pool.SequentialExhaust || !strings.Contains(string(saved), "keep") {
		t.Fatalf("merge lost config: %s", saved)
	}
	_, err = saveConfig([]byte(`{"pool":{"strategy":{"domestic":"bad"}}}`), fp, live, p, up, sch)
	if err == nil {
		t.Fatal("invalid strategy accepted")
	}
	after, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != string(after) {
		t.Fatal("invalid save changed file")
	}
	if a := p.PickExcludingForRealm(nil, "m", "cn"); a == nil || a.UID != "a" {
		t.Fatalf("invalid save changed strategy: %v", a)
	}
}

// 回归：Docker/Zeabur 用环境变量设密钥、文件仍是样例配置时，面板只改一个字段（模型启停）
// 也不能让热快照退回文件里的 api_key/admin_key。
func TestSaveConfigKeepsEnvKeys(t *testing.T) {
	t.Setenv("WB2A_API_KEY", "594926")
	t.Setenv("WB2A_ADMIN_KEY", "env-admin")
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	if err := os.WriteFile(fp, []byte(`{"api_key":"test_key","admin_key":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	live := livecfg.New(liveFrom(cfg))
	p := pool.New("")
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{})
	if _, err := saveConfig([]byte(`{"models":{"disabled":["cn:x"]}}`), fp, live, p, up, sch); err != nil {
		t.Fatal(err)
	}
	s := live.Load()
	if s.APIKey != "594926" || s.PanelKey() != "env-admin" {
		t.Fatalf("env keys lost after save: api=%q panel=%q", s.APIKey, s.PanelKey())
	}
}

// 面板登录密钥：面板里保存的值优先于环境变量 WB2A_ADMIN_KEY（环境变量只在未设置时兜底），
// 保存后立即生效；清空后回落环境变量。
func TestPanelAdminKeyBeatsEnv(t *testing.T) {
	t.Setenv("WB2A_API_KEY", "594926")
	t.Setenv("WB2A_ADMIN_KEY", "env-admin")
	dir := t.TempDir()
	fp := filepath.Join(dir, "config.json")
	if err := os.WriteFile(fp, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	live := livecfg.New(liveFrom(cfg))
	if live.Load().PanelKey() != "env-admin" {
		t.Fatalf("env fallback panel=%q", live.Load().PanelKey())
	}
	p := pool.New("")
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{})
	if _, err := saveConfig([]byte(`{"admin_key":"my-panel-key"}`), fp, live, p, up, sch); err != nil {
		t.Fatal(err)
	}
	if got := live.Load().PanelKey(); got != "my-panel-key" {
		t.Fatalf("panel-set key not applied: %q", got)
	}
	if again, _ := Load(fp); again.AdminKey != "my-panel-key" {
		t.Fatalf("panel-set key lost on restart: %q", again.AdminKey)
	}
	if _, err := saveConfig([]byte(`{"admin_key":""}`), fp, live, p, up, sch); err != nil {
		t.Fatal(err)
	}
	if got := live.Load().PanelKey(); got != "env-admin" {
		t.Fatalf("cleared key should fall back to env: %q", got)
	}
}
