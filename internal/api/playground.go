package api

import (
	"bytes"
	"encoding/json"
	"net/http"

	"litegate/internal/store"
)

// 对话测试（管理台 playground）：进程内复用数据面完整链路——虚拟密钥鉴权、
// 渠道选择与故障转移、限流预算、SSE 流式回传与请求日志/成本核算都走同一条路，
// 页面只是管理会话内的一个特殊客户端（X-LiteGate-App=playground，日志可归因）。

const playgroundApp = "playground"

// registerPlayground 挂载对话测试路由（管理会话鉴权与其它管理面接口一致）。
func (a *admin) registerPlayground(mux *http.ServeMux) {
	mux.Handle("GET /api/admin/playground/models", a.auth(a.playgroundModels))
	mux.Handle("POST /api/admin/playground/chat", a.auth(a.playgroundChat))
}

// playgroundModels 返回当前全部启用模型（复用数据面聚合缓存），供下拉选择。
func (a *admin) playgroundModels(w http.ResponseWriter, r *http.Request) {
	if serverProxy == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "data plane not ready"})
		return
	}
	items := serverProxy.aggregateModels(r.Context())
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": ids})
}

// playgroundChat 把对话请求交给数据面：挑一把可用虚拟密钥，强制流式，
// 响应（SSE 或错误 JSON）原样回传给管理页。
func (a *admin) playgroundChat(w http.ResponseWriter, r *http.Request) {
	if serverProxy == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "data plane not ready"})
		return
	}
	var in struct {
		Model     string           `json:"model"`
		Messages  []map[string]any `json:"messages"`
		MaxTokens int64            `json:"max_tokens"`
	}
	if readJSON(w, r, &in) != nil {
		return
	}
	if in.Model == "" || len(in.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model and messages are required"})
		return
	}
	if in.MaxTokens <= 0 {
		// 思考型模型的思维链会吃掉过小的 max_tokens 预算导致正文为空，默认给足
		in.MaxTokens = 2048
	}
	if in.MaxTokens > 32768 {
		in.MaxTokens = 32768
	}
	key, reason := pickPlaygroundKey(a.st, in.Model)
	if key == "" {
		if reason != "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": reason})
		} else {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "没有可用的启用虚拟密钥，请先在「虚拟密钥」页签发"})
		}
		return
	}
	body, err := json.Marshal(map[string]any{
		"model":      in.Model,
		"messages":   in.Messages,
		"max_tokens": in.MaxTokens,
		"stream":     true,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-LiteGate-App", playgroundApp)
	serverProxy.serveOpenAI(w, req)
}

// pickPlaygroundKey 选一把能调该模型的启用虚拟密钥：优先不设白名单的，
// 其次白名单包含该模型的。返回 (密钥, 原因)；有启用密钥但白名单都不含该模型时
// reason 给出可操作的提示，密钥明文本就在库中（reveal 同源）。
func pickPlaygroundKey(st *store.Store, model string) (string, string) {
	keys, err := st.ListAPIKeys()
	if err != nil {
		return "", ""
	}
	fallback := ""
	enabled := false
	for i := range keys {
		k := &keys[i]
		if !k.Enabled {
			continue
		}
		enabled = true
		if len(k.AllowedModels) == 0 {
			return k.Key, ""
		}
		if fallback == "" && k.AllowsModel(model) {
			fallback = k.Key
		}
	}
	if enabled && fallback == "" {
		return "", "启用密钥的模型白名单均未包含 " + model + "，请到「虚拟密钥」页调整白名单"
	}
	return fallback, ""
}
