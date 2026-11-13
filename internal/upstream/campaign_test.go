package upstream

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseActivityKnownAndProgress(t *testing.T) {
	data := json.RawMessage(`{
	 "tasks":[
	  {"task_code":"ai_chat","target_count":3,"need_view":false,"reward_credit":100},
	  {"task_code":"expert","target_count":1,"reward_credit":100},
	  {"task_code":"app","target_count":1,"reward_credit":100}],
	 "progress":[
	  {"task_code":"ai_chat","progress":3,"completed_at":"2026-11-13T10:00:00Z","claimed_at":null},
	  {"task_code":"expert","progress":1,"completed_at":1763000000,"claimed_at":1763000100}]}`)
	act, err := parseActivity("campaign_starbucks_2026", data)
	if err != nil {
		t.Fatal(err)
	}
	if act.Name != "星巴克协作" || len(act.Tasks) != 3 {
		t.Fatalf("act=%+v", act)
	}
	byCode := map[string]ActivityTask{}
	for _, x := range act.Tasks {
		byCode[x.Code] = x
	}
	if c := byCode["ai_chat"]; c.Kind != ActivityKindChat || c.Status() != "claimable" || c.NeedView || c.Name != "与 AI 对话 3 次" {
		t.Fatalf("ai_chat=%+v status=%s", c, c.Status())
	}
	if e := byCode["expert"]; e.Status() != "claimed" {
		t.Fatalf("expert status=%s", e.Status())
	}
	if a := byCode["app"]; a.Kind != ActivityKindManual || a.Status() != "manual" {
		t.Fatalf("app=%+v", a)
	}
}

func TestParseActivityUnknownGeneric(t *testing.T) {
	data := json.RawMessage(`{"activity_name":"元旦福利","end_time":"2027-01-03 23:59:59",
	 "tasks":[
	  {"task_code":"chat_twice","task_name":"对话 2 次","target_count":2,"reward_credit":30},
	  {"task_code":"try_expert","task_name":"试试专家","ext":{"expert_id":"ex_abc","expert_name":"写作助手"},"reward_credit":40},
	  {"task_code":"share_poster","task_name":"分享海报","reward_credit":20},
	  {"task_code":"mystery","task_name":"神秘任务","task_type":"scan_qr","reward_credit":10}]}`)
	act, err := parseActivity("campaign_newyear_2027", data)
	if err != nil {
		t.Fatal(err)
	}
	if act.Name != "元旦福利" || act.Deadline == "" {
		t.Fatalf("act=%+v", act)
	}
	want := map[string]string{"chat_twice": "chat", "try_expert": "expert", "share_poster": "manual", "mystery": "manual"}
	for _, x := range act.Tasks {
		if want[x.Code] != x.Kind {
			t.Errorf("%s kind=%s want %s", x.Code, x.Kind, want[x.Code])
		}
	}
	if act.Tasks[1].ExpertID != "ex_abc" || act.Tasks[1].ExpertName != "写作助手" {
		t.Fatalf("ext not read: %+v", act.Tasks[1])
	}
}

func TestParseActivityEmpty(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"tasks":[]}`} {
		act, err := parseActivity("x", json.RawMessage(raw))
		if err != nil || len(act.Tasks) != 0 {
			t.Fatalf("%s: act=%+v err=%v", raw, act, err)
		}
	}
}

func TestHarvestActivityCodes(t *testing.T) {
	raw := []byte(`{"tasks":[{"jump_url":"/pages/x?activity_code=campaign_newyear_2027&from=growth"},
	 {"ext":{"activityCode":"spring_fest"}},{"desc":"参加 campaign_reporter_2026 活动"}]}`)
	got := HarvestActivityCodes(raw)
	want := []string{"campaign_newyear_2027", "campaign_reporter_2026", "spring_fest"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestNormDeadline(t *testing.T) {
	if got := normDeadline("1767196799000"); got != "2025-12-31T23:59:59+08:00" {
		t.Fatalf("ms: %s", got)
	}
	if got := normDeadline("2027-01-03"); got != "2027-01-03T00:00:00+08:00" {
		t.Fatalf("date: %s", got)
	}
}
