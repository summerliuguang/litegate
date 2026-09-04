package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"litegate/internal/store"
)

func newTestServer(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return NewServer(st, "testpw", nil), st
}

func mustCreateChannel(t *testing.T, st *store.Store, typ, base string, models []string, priority int) int64 {
	t.Helper()
	id, err := st.CreateChannel(&store.Channel{
		Name: "ch-" + typ + "-" + base, Type: typ, BaseURL: base,
		APIKey: "up-key", Models: models, Weight: 1, Priority: priority, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return id
}

func mustCreateKey(t *testing.T, st *store.Store) string {
	t.Helper()
	k, err := st.CreateAPIKey("test")
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	return k.Key
}

func do(srv http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestProxyRequiresAPIKey(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	rec = do(srv, "POST", "/v1/chat/completions", `{"model":"m"}`,
		map[string]string{"Authorization": "Bearer sk-lg-wrong"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAdminAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := do(srv, "POST", "/api/admin/login", `{"password":"testpw"}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d", rec.Code)
	}
	var login struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil || login.Token == "" {
		t.Fatalf("login response: %s", rec.Body.String())
	}
	rec = do(srv, "GET", "/api/admin/channels", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("channels without token = %d, want 401", rec.Code)
	}
	rec = do(srv, "GET", "/api/admin/channels", "",
		map[string]string{"Authorization": "Bearer " + login.Token})
	if rec.Code != http.StatusOK {
		t.Fatalf("channels with token = %d", rec.Code)
	}
	rec = do(srv, "POST", "/api/admin/login", `{"password":"wrong"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, want 401", rec.Code)
	}
}

func TestOpenAIChatPassthrough(t *testing.T) {
	var gotPath, gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", upstream.URL+"/v1", nil, 0)
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/v1/chat/completions",
		`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("upstream path = %s", gotPath)
	}
	if gotAuth != "Bearer up-key" {
		t.Fatalf("upstream auth = %s", gotAuth)
	}
	if !strings.Contains(rec.Body.String(), `"hi"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestOpenAIStreamForwarding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			io.WriteString(w, "data: {\"delta\":\""+string(rune('a'+i))+"\"}\n\n")
			f.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", upstream.URL+"/v1", nil, 0)
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/v1/chat/completions",
		`{"model":"m","stream":true,"messages":[]}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"delta":"a"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream body incomplete: %s", body)
	}
}

func TestAnthropicPassthrough(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-Api-Key")
		gotVersion = r.Header.Get("Anthropic-Version")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","content":[{"type":"text","text":"hey"}]}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "anthropic", upstream.URL+"/v1", nil, 0)
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/v1/messages",
		`{"model":"claude-x","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"X-Api-Key": key}) // Claude Code 以 x-api-key 携带网关密钥
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %s", gotPath)
	}
	if gotKey != "up-key" {
		t.Fatalf("upstream x-api-key = %s", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Fatalf("anthropic-version = %s", gotVersion)
	}
}

func TestFailoverOnUpstreamError(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"error":"boom"}`)
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"ok"}`)
	}))
	defer good.Close()

	srv, st := newTestServer(t)
	// bad 渠道优先级更高，必然先被选中
	mustCreateChannel(t, st, "openai", bad.URL+"/v1", []string{"m"}, 10)
	mustCreateChannel(t, st, "openai", good.URL+"/v1", []string{"m"}, 0)
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m"}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestAllChannelsFail(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", bad.URL+"/v1", nil, 0)
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m"}`,
		map[string]string{"Authorization": "Bearer " + key})
	// 唯一渠道返回 500：无可转移目标，网关原样透传上游状态码
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestNoChannelForModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", upstream.URL+"/v1", []string{"other-model"}, 0)
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/v1/chat/completions", `{"model":"missing"}`,
		map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestModelsAggregation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"m-a"},{"id":"m-b"}]}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", upstream.URL+"/v1", nil, 0)
	key := mustCreateKey(t, st)

	rec := do(srv, "GET", "/v1/models", "", map[string]string{"Authorization": "Bearer " + key})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "m-a") || !strings.Contains(rec.Body.String(), "m-b") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestChannelKeyMasking(t *testing.T) {
	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", "http://example.invalid/v1", nil, 0)
	key := mustCreateKey(t, st)

	rec := do(srv, "POST", "/api/admin/login", `{"password":"testpw"}`, nil)
	var login struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &login)

	rec = do(srv, "GET", "/api/admin/channels", "",
		map[string]string{"Authorization": "Bearer " + login.Token})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "up-key") {
		t.Fatalf("channel api_key leaked: %s", body)
	}
	// "up-key" 长度 ≤ 8，应整体打码
	if !strings.Contains(body, `"api_key":"******"`) {
		t.Fatalf("masked key missing: %s", body)
	}
	_ = key
}
