package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"litegate/internal/store"
)

func TestLargeResponseAccounting(t *testing.T) {
	for _, first := range []bool{false, true} {
		t.Run(map[bool]string{false: "usage-last", true: "usage-first"}[first], func(t *testing.T) {
			usage := `"usage":{"prompt_tokens":1000,"completion_tokens":500}`
			content := `"choices":[{"message":{"content":"` + strings.Repeat("x", 256*1024) + `"}}]`
			body := "{" + content + "," + usage + "}"
			if first {
				body = "{" + usage + "," + content + "}"
			}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, body)
			}))
			defer up.Close()
			srv, st := newTestServer(t)
			mustCreateChannel(t, st, "openai", up.URL, nil, 0)
			key := mustCreateKey(t, st)
			if err := st.UpsertModelPrice(&store.ModelPrice{Model: "m", InputPrice: 2, OutputPrice: 4}); err != nil {
				t.Fatal(err)
			}
			rec := do(srv, "POST", "/v1/chat/completions", `{"model":"m","messages":[]}`, map[string]string{"Authorization": "Bearer " + key})
			if rec.Code != 200 || rec.Body.String() != body {
				t.Fatal("response changed")
			}
			page, err := st.ListLogs(store.LogFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 || page.Items[0].PromptTokens != 1000 || page.Items[0].CompletionTokens != 500 || page.Items[0].CostUSD != 0.004 {
				t.Fatalf("incorrect accounting: %+v", page.Items)
			}
		})
	}
}

func TestBudgetEnableAndSwitch(t *testing.T) {
	a := newKeyAdmission(nil)
	key := &store.APIKey{ID: 1}
	a.record(key, 10, 20, 1, 7.2)
	for _, period := range []string{"daily", "monthly", "daily"} {
		key.BudgetPeriod = period
		key.BudgetTokens = 30
		if status, _, _ := a.admit(key); status != 429 {
			t.Fatalf("%s lost previous usage", period)
		}
	}
	key.BudgetTokens = 0
	key.BudgetUSD = 0
	a.record(key, 5, 5, 1, 0)
	key.BudgetUSD = 3
	key.BudgetPeriod = "monthly"
	if status, _, _ := a.admit(key); status != 429 {
		t.Fatal("budget disabled/re-enabled lost costs")
	}
}

func TestAudioTranscriptionJSONHeader(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "requires JSON", 415)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"hello"}}]}`)
	}))
	defer up.Close()
	srv, st := newTestServer(t)
	mustCreateChannel(t, st, "openai", up.URL, nil, 0)
	key := mustCreateKey(t, st)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("model", "m")
	f, _ := mw.CreateFormFile("file", "sample.wav")
	f.Write([]byte("audio"))
	mw.Close()
	rec := do(srv, "POST", "/v1/audio/transcriptions", body.String(), map[string]string{"Authorization": "Bearer " + key, "Content-Type": mw.FormDataContentType()})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "hello") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestSSEOverlongLineRecovers(t *testing.T) {
	var s sseUsageScanner
	chunk := bytes.Repeat([]byte("x"), 4096)
	for i := 0; i < 1024; i++ {
		s.Write(chunk)
		if len(s.carry) > 64*1024 {
			t.Fatalf("unbounded carry: %d", len(s.carry))
		}
	}
	s.Write([]byte("\ndata: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n"))
	if !s.found || s.usage.prompt != 3 {
		t.Fatalf("failed to recover: %+v", s.usage)
	}
}

func TestJSONUsageScannerBoundaries(t *testing.T) {
	cases := []struct {
		body   string
		prompt int64
	}{
		{`{"choices":[{"usage":{"prompt_tokens":999},"text":"escaped \\\" usage"}],"us\u0061ge":{"input_tokens":12,"output_tokens":3,"cache_read_input_tokens":4}}`, 16},
		{`{"usage":{"prompt_tokens":7},"text":"` + strings.Repeat("x", 200000) + `"}`, 7},
		{`{"usage":{"ignored":"` + strings.Repeat("x", 100000) + `","prompt_tokens":7}}`, 0},
		{`{"message":{"usage":{"input_tokens":8,"output_tokens":2}}}`, 8},
		{`{"usage":{"prompt_tokens":7},"message":{"usage":{"input_tokens":8}}}`, 7},
		{`{"text":"usage","nested":{"usage":{"prompt_tokens":9}}}`, 0},
	}
	for _, tc := range cases {
		for _, size := range []int{1, 7, 4096} {
			var s jsonUsageScanner
			for i := 0; i < len(tc.body); i += size {
				end := min(i+size, len(tc.body))
				s.Write([]byte(tc.body[i:end]))
				if len(s.buf) > maxUsageTail || len(s.key) > 128 {
					t.Fatal("unbounded buffer")
				}
			}
			if s.usage.prompt != tc.prompt {
				t.Fatalf("chunk=%d got=%+v want=%d", size, s.usage, tc.prompt)
			}
		}
	}
}

func BenchmarkJSONUsageScanner(b *testing.B) {
	body := []byte(`{"choices":[{"message":{"content":"` + strings.Repeat("x", 1024*1024) + `"}}],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var s jsonUsageScanner
		for start := 0; start < len(body); start += 32768 {
			s.Write(body[start:min(start+32768, len(body))])
		}
		if s.usage.prompt != 1000 {
			b.Fatal("lost usage")
		}
	}
}

func TestAudioConversionPreservesUsage(t *testing.T) {
	for _, kind := range []string{"json", "text", "speech"} {
		t.Run(kind, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"choices":[{"message":{"content":"hello","audio":{"data":"aGVsbG8="}}}],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`)
			}))
			defer up.Close()
			srv, st := newTestServer(t)
			mustCreateChannel(t, st, "openai", up.URL, nil, 0)
			key := mustCreateKey(t, st)
			if err := st.UpsertModelPrice(&store.ModelPrice{Model: "m", InputPrice: 2, OutputPrice: 4}); err != nil {
				t.Fatal(err)
			}
			path, body, ct := "/v1/audio/speech", `{"model":"m","input":"hi","voice":"alloy"}`, "application/json"
			if kind != "speech" {
				var buf bytes.Buffer
				mw := multipart.NewWriter(&buf)
				mw.WriteField("model", "m")
				mw.WriteField("response_format", kind)
				f, _ := mw.CreateFormFile("file", "sample.wav")
				f.Write([]byte("audio"))
				mw.Close()
				path, body, ct = "/v1/audio/transcriptions", buf.String(), mw.FormDataContentType()
			}
			rec := do(srv, "POST", path, body, map[string]string{"Authorization": "Bearer " + key, "Content-Type": ct})
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), "hello") {
				t.Fatalf("response: %d %s", rec.Code, rec.Body.String())
			}
			page, err := st.ListLogs(store.LogFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 || page.Items[0].PromptTokens != 1000 || page.Items[0].CostUSD != 0.004 {
				t.Fatalf("audio usage lost: %+v", page.Items)
			}
		})
	}
}

func FuzzJSONUsageScannerEnvelope(f *testing.F) {
	f.Add("escaped \" usage \\ newline\n", uint8(7))
	f.Add("", uint8(1))
	f.Fuzz(func(t *testing.T, content string, chunk uint8) {
		if len(content) > 16384 {
			t.Skip()
		}
		body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": content}, "usage": map[string]any{"prompt_tokens": 999}}}, "message": map[string]any{"usage": map[string]any{"input_tokens": 4}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5}})
		if err != nil {
			t.Fatal(err)
		}
		var scan jsonUsageScanner
		size := int(chunk) + 1
		for start := 0; start < len(body); start += size {
			scan.Write(body[start:min(start+size, len(body))])
		}
		if scan.usage.prompt != 10 || scan.usage.completion != 5 {
			t.Fatalf("incorrect extraction: %+v", scan.usage)
		}
	})
}

func TestAudioResponseBoundAndClose(t *testing.T) {
	body := &trackedAudioBody{Reader: strings.NewReader(strings.Repeat("x", maxBodyBytes+1))}
	if _, err := readAudioResponse(body); err == nil {
		t.Fatal("oversized conversion accepted")
	}
	if !body.closed {
		t.Fatal("upstream body not closed")
	}
}

type trackedAudioBody struct {
	io.Reader
	closed bool
}

func (b *trackedAudioBody) Close() error { b.closed = true; return nil }

func TestInvalidUsageCannotCreditBudget(t *testing.T) {
	for _, body := range []string{
		`{"usage":{"prompt_tokens":-10,"completion_tokens":-20}}`,
		`{"usage":{"prompt_tokens":10,"completion_tokens":-20}}`,
		`{"usage":{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":-1}}}`,
		`{"usage":{"input_tokens":9223372036854775807,"cache_read_input_tokens":1}}`,
		`{"usage":{"prompt_tokens":9223372036854775807,"completion_tokens":1}}`,
	} {
		var scan jsonUsageScanner
		scan.Write([]byte(body))
		if scan.usage != (tokenUsage{}) {
			t.Errorf("invalid usage accepted: %s => %+v", body, scan.usage)
		}
		if usage, ok := usageFromJSON([]byte(body)); ok || usage != (tokenUsage{}) {
			t.Errorf("SSE parser accepted invalid usage: %s => %+v", body, usage)
		}
	}
}

// Concurrent admitted requests can exceed a soft budget, but every completion
// must be counted exactly once and all subsequent admissions must be rejected.
func TestConcurrentBudgetCompletionAccounting(t *testing.T) {
	a := newKeyAdmission(nil)
	key := &store.APIKey{ID: 1, BudgetTokens: 1, BudgetPeriod: "daily"}
	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		if status, _, _ := a.admit(key); status != 0 {
			t.Fatal("unexpected reservation")
		}
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); a.record(key, 10, 20, 1, 7.2) }()
	}
	wg.Wait()
	for _, period := range []string{"daily", "monthly"} {
		copyKey := *key
		copyKey.BudgetPeriod = period
		usd, cny, tokens := a.budgetUsage(&copyKey)
		if tokens != n*30 || usd < 199.999 || usd > 200.001 || cny < 719.999 || cny > 720.001 {
			t.Fatalf("lost concurrent accounting: %s %f %f %d", period, usd, cny, tokens)
		}
		if status, _, _ := a.admit(&copyKey); status != 429 {
			t.Fatal("spent budget admitted")
		}
	}
}
