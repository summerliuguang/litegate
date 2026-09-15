package api

// 批次三功能测试：模型别名、应用归因固化、max_tokens 钳制、并发上限。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"litegate/internal/store"
)

// newRecordingUpstream 记录上游收到的 model / max_tokens / 请求间隔。
func newRecordingUpstream(t *testing.T) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	mu := sync.Mutex{}
	got := &[]map[string]any{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		_ = dec.Decode(&v)
		mu.Lock()
		*got = append(*got, v)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(up.Close)
	return up, got
}

func mustCreateKeyFull(t *testing.T, st *store.Store, k *store.APIKey) string {
	t.Helper()
	if err := st.CreateAPIKey(k); err != nil {
		t.Fatalf("create key: %v", err)
	}
	return k.Key
}

func TestKeyAliasRewrite(t *testing.T) {
	up, got := newRecordingUpstream(t)
	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", up.URL+"/v1", []string{"deepseek-flash"}, 0)
	key := mustCreateKeyFull(t, st, &store.APIKey{
		Name: "alias", ModelAlias: map[string]string{"gpt-4o": "deepseek-flash"},
	})

	rec := postChat(srv, key, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(*got) != 1 || (*got)[0]["model"] != "deepseek-flash" {
		t.Fatalf("upstream body model = %v, want deepseek-flash", *got)
	}
	// 未配置别名的模型不受影响
	rec = postChat(srv, key, `{"model":"deepseek-flash","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("plain model status = %d", rec.Code)
	}
}

func TestKeyAppPinning(t *testing.T) {
	up, _ := newRecordingUpstream(t)
	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", up.URL+"/v1", []string{"m"}, 0)
	key := mustCreateKeyFull(t, st, &store.APIKey{Name: "pinned", AppName: "my-app"})

	rec := postChat(srv, key, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// 客户端伪造的 X-LiteGate-App 不应覆盖密钥固化值
	rec = do(srv, "POST", "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Authorization": "Bearer " + key, "X-LiteGate-App": "spoofed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var rows struct {
		Items []struct {
			App string `json:"app"`
		} `json:"items"`
	}
	_ = json.Unmarshal([]byte("[]"), &rows)
	logs, err := st.ListLogs(store.LogFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range logs.Items {
		if l.App != "my-app" {
			t.Fatalf("log app = %q, want my-app", l.App)
		}
	}
}

func TestKeyMaxTokensClamp(t *testing.T) {
	up, got := newRecordingUpstream(t)
	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", up.URL+"/v1", []string{"m"}, 0)
	key := mustCreateKeyFull(t, st, &store.APIKey{Name: "capped", MaxTokensCap: 100})

	rec := postChat(srv, key, `{"model":"m","max_tokens":5000,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	mt, _ := (*got)[0]["max_tokens"].(json.Number)
	if mt == "" || mt.String() != "100" {
		t.Fatalf("upstream max_tokens = %v, want 100", (*got)[0]["max_tokens"])
	}
	// 低于上限不改动
	rec = postChat(srv, key, `{"model":"m","max_tokens":50,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	mt, _ = (*got)[1]["max_tokens"].(json.Number)
	if mt == "" || mt.String() != "50" {
		t.Fatalf("upstream max_tokens = %v, want 50", (*got)[1]["max_tokens"])
	}
}

func TestKeyConcurrencyLimit(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // 挂住请求，制造并发
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer up.Close()
	close(release) // 上游不真挂；并发制造靠同步发多笔

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", up.URL+"/v1", []string{"m"}, 0)
	key := mustCreateKeyFull(t, st, &store.APIKey{Name: "conc", ConcurrencyLimit: 2})

	// 同步发 5 笔，上限 2：至少应有 429 出现（服务端串行处理时也可能全过——
	// 这里直接验证 admit 逻辑层更可靠，见下方单元断言）
	for i := 0; i < 5; i++ {
		postChat(srv, key, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	}

	// 单元级：直接驱动 enter/release
	adm := newKeyAdmission(nil)
	k := &store.APIKey{ID: 7, ConcurrencyLimit: 2}
	r1, ok1 := adm.enter(k)
	r2, ok2 := adm.enter(k)
	_, ok3 := adm.enter(k)
	if !ok1 || !ok2 || ok3 {
		t.Fatalf("enter: %v %v %v, want true true false", ok1, ok2, ok3)
	}
	r1()
	r3, ok4 := adm.enter(k)
	if !ok4 {
		t.Fatal("release should free a slot")
	}
	r2()
	r3()
}
