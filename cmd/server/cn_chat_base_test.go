package main

import (
	"slices"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// 老配置（无 cn 段）必须保留内置 CN base，零回归。
func TestCNChatBaseDefaultKeepsBuiltin(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	up := upstream.New()
	want := up.ChatBaseCN
	applyCNChatBase(up, cfg)
	if up.ChatBaseCN != want {
		t.Fatalf("ChatBaseCN=%q want builtin %q", up.ChatBaseCN, want)
	}
}

func TestCNChatBaseOverride(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"cn":{"chat_base":"https://www.workbuddy.cn"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	up := upstream.New()
	applyCNChatBase(up, cfg)
	if up.ChatBaseCN != "https://www.workbuddy.cn" {
		t.Fatalf("ChatBaseCN=%q want override", up.ChatBaseCN)
	}
	// global base 不受 cn 覆盖影响。
	if up.ChatBaseGlobal != "" {
		t.Fatalf("ChatBaseGlobal=%q want untouched", up.ChatBaseGlobal)
	}
}

// cn.chat_base 只在装配期生效，面板保存后须提示重启。
func TestCNChatBaseRequiresRestart(t *testing.T) {
	cfg := Default()
	if !slices.Contains(restartRequiredFields(cfg), "cn.chat_base") {
		t.Fatal("cn.chat_base missing from restart-required fields")
	}
}

// 只改热生效字段（如模型启停）时不提示需重启；真改了需重启字段才列出。
func TestChangedRestartFieldsOnlyReportsChanges(t *testing.T) {
	boot := Default()
	_ = boot.normalize()
	same := Default()
	_ = same.normalize()
	same.Models.Disabled = []string{"cn:x"}
	if got := changedRestartFields(boot, same); len(got) != 0 {
		t.Fatalf("unchanged restart fields reported: %v", got)
	}
	same.CN.ChatBase = "https://www.workbuddy.cn"
	if got := changedRestartFields(boot, same); !slices.Equal(got, []string{"cn.chat_base"}) {
		t.Fatalf("got %v want [cn.chat_base]", got)
	}
}
