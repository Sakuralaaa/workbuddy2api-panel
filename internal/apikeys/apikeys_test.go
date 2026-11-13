package apikeys

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateVerifyLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-keys.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	k, secret, err := s.Create("客户端 A")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "sk-wb-") || k.Display == secret || strings.Contains(k.Display, secret[9:len(secret)-4]) {
		t.Fatalf("secret/display wrong: %q %q", secret, k.Display)
	}
	if got, ok := s.Verify(secret); !ok || got.ID != k.ID {
		t.Fatalf("verify failed: %+v %v", got, ok)
	}
	if _, ok := s.Verify(secret + "x"); ok {
		t.Fatal("wrong secret accepted")
	}
	if err := s.Rename(k.ID, "改名后"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnabled(k.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Verify(secret); ok {
		t.Fatal("disabled key accepted")
	}
	// 重启：从磁盘恢复（密钥表立即落盘，明文不落盘）
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l := s2.List()
	if len(l) != 1 || l[0].Name != "改名后" || l[0].Enabled {
		t.Fatalf("reload mismatch: %+v", l)
	}
	if err := s2.SetEnabled(k.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Verify(secret); !ok {
		t.Fatal("re-enabled key rejected after reload")
	}
	if err := s2.Delete(k.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Verify(secret); ok || s2.Count() != 0 {
		t.Fatal("deleted key still valid")
	}
	if err := s2.Delete(k.ID); err != ErrNotFound {
		t.Fatalf("delete twice err=%v", err)
	}
}

func TestUsageReportByDayAndModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api-keys.json")
	s, _ := Open(path)
	k, _, _ := s.Create("A")
	day1 := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)  // CST 11:00 11-10
	day2 := time.Date(2026, 11, 10, 17, 0, 0, 0, time.UTC) // CST 01:00 11-11
	s.Record(Usage{KeyID: k.ID, Time: day1, Model: "cn:glm-5.2", OK: true, PromptTokens: 10, CompletionTokens: 5, Credits: 0.5})
	s.Record(Usage{KeyID: k.ID, Time: day1, Model: "cn:glm-5.2", OK: false})
	s.Record(Usage{KeyID: k.ID, Time: day2, Model: "cn:kimi", OK: true, TotalTokens: 100, Credits: 1})
	s.Record(Usage{KeyID: LegacyID, Time: day2, Model: "cn:kimi", OK: true, TotalTokens: 7})
	s.Record(Usage{KeyID: "", Time: day2, Model: "x", OK: true}) // 未鉴权：不计
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, _ := Open(path)
	rep := s2.Report("2026-11-10", "2026-11-11")
	if rep.Total.Requests != 4 || rep.Total.Success != 3 || rep.Total.Failed != 1 {
		t.Fatalf("total=%+v", rep.Total)
	}
	var a, legacy *KeyReport
	for i := range rep.Keys {
		switch rep.Keys[i].ID {
		case k.ID:
			a = &rep.Keys[i]
		case LegacyID:
			legacy = &rep.Keys[i]
		}
	}
	if a == nil || legacy == nil {
		t.Fatalf("keys missing: %+v", rep.Keys)
	}
	if a.ByDay["2026-11-10"].Requests != 2 || a.ByDay["2026-11-11"].Requests != 1 {
		t.Fatalf("by day=%+v", a.ByDay)
	}
	g := a.ByModel["cn:glm-5.2"]
	if g.Success != 1 || g.Failed != 1 || g.TotalTokens != 15 || g.Credits != 0.5 {
		t.Fatalf("glm bucket=%+v", g)
	}
	if a.Total.TotalTokens != 115 || a.Total.Credits != 1.5 {
		t.Fatalf("a total=%+v", a.Total)
	}
	if legacy.Name != LegacyName {
		t.Fatalf("legacy name=%q", legacy.Name)
	}
	// 区间外不计
	if r := s2.Report("2026-11-11", "2026-11-11"); r.Total.Requests != 2 {
		t.Fatalf("range filter total=%+v", r.Total)
	}
	if lu := s2.List()[0].LastUsedAt; !lu.Equal(day2) {
		t.Fatalf("last used=%v", lu)
	}
}
