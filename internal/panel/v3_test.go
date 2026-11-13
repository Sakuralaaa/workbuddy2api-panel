package panel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/apikeys"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/panelstore"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
)

func doReq(p *Panel, method, path, key, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	return rec
}

// 管理密钥：admin_key 设置后面板只认 admin_key（调用用的 api_key 与多密钥都进不来）；
// 未设置时回落 api_key；封面探测接口无需鉴权且不泄露数据；热改立即生效。
func TestAdminKeyEnforced(t *testing.T) {
	keys, _ := apikeys.Open("")
	_, callKey, _ := keys.Create("调用")
	live := livecfg.New(livecfg.Snapshot{APIKey: "call-key", AdminKey: "admin-key"})
	p := New(Config{Live: live, Keys: keys, Store: mustStore(t, "")})

	for _, k := range []string{"", "call-key", callKey, "wrong"} {
		if rec := doReq(p, "GET", "/panel/api/keys", k, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("key %q reached panel api: %d", k, rec.Code)
		}
		if rec := doReq(p, "GET", "/panel/api/auth/check", k, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("auth/check accepted %q", k)
		}
	}
	if rec := doReq(p, "GET", "/panel/api/auth/check", "admin-key", ""); rec.Code != 200 {
		t.Fatalf("admin key rejected: %d", rec.Code)
	}
	rec := doReq(p, "GET", "/panel/api/auth/info", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"auth_required":true`) {
		t.Fatalf("auth/info=%d %s", rec.Code, rec.Body)
	}
	// admin_key 清空 → 回落 api_key
	live.Store(livecfg.Snapshot{APIKey: "call-key"})
	if rec := doReq(p, "GET", "/panel/api/auth/check", "call-key", ""); rec.Code != 200 {
		t.Fatalf("fallback to api_key failed: %d", rec.Code)
	}
	if rec := doReq(p, "GET", "/panel/api/auth/check", "admin-key", ""); rec.Code != 401 {
		t.Fatalf("stale admin key accepted: %d", rec.Code)
	}
	// 都为空 → 不鉴权
	live.Store(livecfg.Snapshot{})
	if rec := doReq(p, "GET", "/panel/api/auth/info", "", ""); !strings.Contains(rec.Body.String(), `"auth_required":false`) {
		t.Fatalf("auth/info no-auth=%s", rec.Body)
	}
}

func mustStore(t *testing.T, path string) *panelstore.Store {
	t.Helper()
	s, err := panelstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// 密钥管理接口：创建只返回一次明文、改名、停用、删除、用量报表。
func TestKeysAPI(t *testing.T) {
	keys, _ := apikeys.Open("")
	p := New(Config{APIKey: "adm", Keys: keys})
	rec := doReq(p, "POST", "/panel/api/keys", "adm", `{"name":"手机"}`)
	var created struct {
		Key    apikeys.Public `json:"key"`
		Secret string         `json:"secret"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if rec.Code != 200 || created.Secret == "" || created.Key.Name != "手机" {
		t.Fatalf("create=%d %s", rec.Code, rec.Body)
	}
	list := doReq(p, "GET", "/panel/api/keys", "adm", "")
	if strings.Contains(list.Body.String(), created.Secret) || strings.Contains(list.Body.String(), `"hash"`) {
		t.Fatalf("list leaks secret/hash: %s", list.Body)
	}
	id := created.Key.ID
	if rec := doReq(p, "POST", "/panel/api/keys/"+id+"/rename", "adm", `{"name":"平板"}`); rec.Code != 200 {
		t.Fatalf("rename=%d", rec.Code)
	}
	if rec := doReq(p, "POST", "/panel/api/keys/"+id+"/enabled", "adm", `{"enabled":false}`); rec.Code != 200 {
		t.Fatalf("disable=%d", rec.Code)
	}
	if _, ok := keys.Verify(created.Secret); ok {
		t.Fatal("disabled via api still valid")
	}
	keys.Record(apikeys.Usage{KeyID: id, Model: "glm-5.2", OK: true, TotalTokens: 9})
	rec = doReq(p, "GET", "/panel/api/keys/usage?days=7", "adm", "")
	if !strings.Contains(rec.Body.String(), `"平板"`) || !strings.Contains(rec.Body.String(), `"total_tokens":9`) {
		t.Fatalf("usage=%s", rec.Body)
	}
	if rec := doReq(p, "POST", "/panel/api/keys/"+id+"/delete", "adm", ""); rec.Code != 200 {
		t.Fatalf("delete=%d", rec.Code)
	}
	if rec := doReq(p, "POST", "/panel/api/keys/"+id+"/delete", "adm", ""); rec.Code != 404 {
		t.Fatalf("delete twice=%d", rec.Code)
	}
}

// 模型启停接口：经 SaveConfig 写 models.disabled（归一化为带域前缀的精确 id）。
func TestModelsToggleAPI(t *testing.T) {
	live := livecfg.New(livecfg.Snapshot{})
	var saved []byte
	p := New(Config{Live: live, SaveConfig: func(raw []byte) ([]string, error) {
		saved = raw
		var m struct {
			Models struct {
				Disabled []string `json:"disabled"`
			} `json:"models"`
		}
		_ = json.Unmarshal(raw, &m)
		live.Store(livecfg.Snapshot{DisabledModels: livecfg.NormalizeHiddenModels(m.Models.Disabled)})
		return nil, nil
	}})
	if rec := doReq(p, "POST", "/panel/api/models/toggle", "", `{"id":"cn:glm-5.2","enabled":false}`); rec.Code != 200 {
		t.Fatalf("toggle off=%d %s", rec.Code, rec.Body)
	}
	if !bytes.Contains(saved, []byte(`"disabled":["cn:glm-5.2"]`)) {
		t.Fatalf("saved=%s", saved)
	}
	doReq(p, "POST", "/panel/api/models/toggle", "", `{"id":"global:kimi","enabled":false}`)
	doReq(p, "POST", "/panel/api/models/toggle", "", `{"id":"cn:glm-5.2","enabled":true}`)
	if !bytes.Contains(saved, []byte(`"disabled":["global:kimi"]`)) {
		t.Fatalf("saved after re-enable=%s", saved)
	}
}

// 备注与任务记录服务端持久化：写入后重开存储仍在；旧 localStorage 8 位前缀备注可迁移。
func TestNotesAndRecordsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel-store.json")
	st := mustStore(t, path)
	pl := pool.New("")
	p := New(Config{Store: st, Pool: pl})
	if rec := doReq(p, "POST", "/panel/api/accounts/uid-123456789/note", "", `{"note":"主力号"}`); rec.Code != 200 {
		t.Fatalf("note=%d %s", rec.Code, rec.Body)
	}
	obs := NewStoreObserver(st)
	obs.JobResult(scheduler.JobResult{RunID: "r1", Trigger: "schedule", Job: scheduler.JobTravel, UID: "uid-123456789", OK: true, Outcome: "claimed", Message: "旅行归来领奖 +30 积分", Credits: 30})
	obs.RunDone(scheduler.RunReport{ID: "r1", Job: scheduler.JobTravel, Trigger: "schedule", Accounts: 1, OK: 1, Credits: 30})
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2 := mustStore(t, path)
	p2 := New(Config{Store: st2, Pool: pl})
	rec := doReq(p2, "GET", "/panel/api/notes", "", "")
	if !strings.Contains(rec.Body.String(), "主力号") {
		t.Fatalf("notes after restart=%s", rec.Body)
	}
	rec = doReq(p2, "GET", "/panel/api/task_records?uid=uid-123456789", "", "")
	if !strings.Contains(rec.Body.String(), "旅行归来领奖") || !strings.Contains(rec.Body.String(), `"job_name":"猫猫旅行"`) {
		t.Fatalf("records after restart=%s", rec.Body)
	}
	rec = doReq(p2, "GET", "/panel/api/runs", "", "")
	if !strings.Contains(rec.Body.String(), `"id":"r1"`) {
		t.Fatalf("runs=%s", rec.Body)
	}
	rec = doReq(p2, "GET", "/panel/api/runs/r1", "", "")
	if !strings.Contains(rec.Body.String(), "旅行归来领奖") {
		t.Fatalf("run detail=%s", rec.Body)
	}
	doReq(p2, "POST", "/panel/api/notes", "", "")
	// 清空历史不动备注
	doReq(p2, "POST", "/panel/api/runs/clear", "", "")
	if len(st2.Records("", "", 0)) != 0 || st2.Note("uid-123456789") != "主力号" {
		t.Fatal("clear history wrong")
	}
	// 删除备注
	doReq(p2, "POST", "/panel/api/accounts/uid-123456789/note", "", `{"note":""}`)
	if st2.Note("uid-123456789") != "" {
		t.Fatal("note not cleared")
	}
}
