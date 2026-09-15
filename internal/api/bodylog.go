package api

// 日志正文采样留存：按采样率保存非流式请求的请求体与响应尾部（复用 usage
// 解析的 64KB 尾缓冲，零额外拷贝），供管理台"回放"排障——用存储的请求体在
// 进程内重打一次数据面（白名单旁路同 playground），对比结果定位问题。
// 配置存 settings 表；表按 TTL 清理。

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"

	"litegate/internal/store"
)

const settingsKeyBodyLog = "body_log_config"

// BodyLogConfig 是正文采样配置；SamplePct=0 或 Enabled=false 时完全不采集。
type BodyLogConfig struct {
	Enabled   bool `json:"enabled"`
	SamplePct int  `json:"sample_pct"` // 1-100
	TtlDays   int  `json:"ttl_days"`   // 留存天数
	MaxBytes  int  `json:"max_bytes"`  // 单份正文截断上限
}

func (c *BodyLogConfig) normalize() {
	if c.SamplePct <= 0 || c.SamplePct > 100 {
		c.SamplePct = 10
	}
	if c.TtlDays <= 0 {
		c.TtlDays = 7
	}
	if c.MaxBytes <= 0 || c.MaxBytes > 64<<10 {
		c.MaxBytes = 32 << 10
	}
}

// bodyLogCfgCache 60s 缓存配置，避免每笔请求读一次 settings 表。
type bodyLogCfgCache struct {
	mu      sync.Mutex
	cfg     BodyLogConfig
	expires time.Time
}

func (p *proxy) bodyLogCfg() BodyLogConfig {
	p.bcfg.mu.Lock()
	defer p.bcfg.mu.Unlock()
	if time.Now().Before(p.bcfg.expires) {
		return p.bcfg.cfg
	}
	var cfg BodyLogConfig
	if raw, err := p.st.GetSetting(settingsKeyBodyLog); err == nil {
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	cfg.normalize()
	p.bcfg.cfg = cfg
	p.bcfg.expires = time.Now().Add(60 * time.Second)
	return cfg
}

// InvalidateBodyLogCfg 供管理面改配置后调用（经 admin.invalidateBodyLog 引用）。
func (p *proxy) invalidateBodyLogCfg() {
	p.bcfg.mu.Lock()
	p.bcfg.expires = time.Time{}
	p.bcfg.mu.Unlock()
}

// captureBody 采样落库：仅非流式响应、采样命中时。失败只记日志。
func (p *proxy) captureBody(logID int64, ak *store.APIKey, model string, reqBody, respBody []byte) {
	if logID <= 0 {
		return
	}
	cfg := p.bodyLogCfg()
	if !cfg.Enabled || respBody == nil {
		return
	}
	if rand.IntN(100) >= cfg.SamplePct {
		return
	}
	keyID := int64(0)
	if ak != nil {
		keyID = ak.ID
	}
	if len(reqBody) > cfg.MaxBytes {
		reqBody = reqBody[:cfg.MaxBytes]
	}
	if len(respBody) > cfg.MaxBytes {
		respBody = respBody[:cfg.MaxBytes]
	}
	if err := p.st.InsertRequestBody(logID, model, keyID, string(reqBody), string(respBody)); err != nil {
		log.Printf("capture body log %d: %v", logID, err)
		return
	}
	if rand.IntN(50) == 0 {
		if n, err := p.st.PruneRequestBodies(cfg.TtlDays); err == nil && n > 0 {
			log.Printf("bodylog pruned %d rows", n)
		}
	}
}

// ---- 管理端点 ----

func (a *admin) getBodyLog(w http.ResponseWriter, _ *http.Request) {
	if a.st == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "store not ready"})
		return
	}
	var cfg BodyLogConfig
	if raw, err := a.st.GetSetting(settingsKeyBodyLog); err == nil {
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	cfg.normalize()
	writeJSON(w, http.StatusOK, cfg)
}

func (a *admin) putBodyLog(w http.ResponseWriter, r *http.Request) {
	var cfg BodyLogConfig
	if readJSON(w, r, &cfg) != nil {
		return
	}
	cfg.normalize()
	raw, err := json.Marshal(cfg)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := a.st.SetSetting(settingsKeyBodyLog, string(raw)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if a.invalidateBodyLog != nil {
		a.invalidateBodyLog()
	}
	a.audit(r, "bodylog.update", fmt.Sprintf("enabled=%v sample=%d%%", cfg.Enabled, cfg.SamplePct))
	writeJSON(w, http.StatusOK, cfg)
}

func (a *admin) getRequestBody(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rb, err := a.st.GetRequestBody(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "body not captured"})
		return
	}
	writeJSON(w, http.StatusOK, rb)
}

// replayBody 用留存的请求体在进程内重打一次数据面（强制非流式，白名单旁路同
// playground），返回解析后的正文与用量，供与原请求对比。
func (a *admin) replayBody(w http.ResponseWriter, r *http.Request) {
	if serverProxy == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "data plane not ready"})
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rb, err := a.st.GetRequestBody(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "body not captured"})
		return
	}
	var bodyMap map[string]any
	dec := json.NewDecoder(strings.NewReader(rb.ReqBody))
	dec.UseNumber()
	if err := dec.Decode(&bodyMap); err != nil || bodyMap == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "stored body is not a valid JSON object"})
		return
	}
	bodyMap["stream"] = false // 回放强制非流式，便于整体取回
	body, err := json.Marshal(bodyMap)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	key := pickPlaygroundKey(a.st)
	if key == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "没有可用的启用虚拟密钥"})
		return
	}
	req, err := http.NewRequestWithContext(playgroundCtx(r), http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-LiteGate-App", "replay")
	rec := httptest.NewRecorder()
	serverProxy.serveOpenAI(rec, req)

	var v struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
		Error any             `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	out := map[string]any{"status": rec.Code}
	if v.Error != nil {
		out["error"] = v.Error
	}
	if len(v.Choices) > 0 {
		out["content"] = v.Choices[0].Message.Content
	}
	if v.Usage != nil {
		out["usage"] = json.RawMessage(v.Usage)
	}
	writeJSON(w, http.StatusOK, out)
}
