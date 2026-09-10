package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"litegate/internal/store"
)

// 对话测试（管理台 playground）：进程内复用数据面完整链路——虚拟密钥鉴权、
// 渠道选择与故障转移、限流预算、SSE 流式回传与请求日志/成本核算都走同一条路，
// 页面只是管理会话内的一个特殊客户端（X-LiteGate-App=playground，日志可归因）。
// 与普通客户端的差异：显式跳过密钥模型白名单（所有启用模型均可测试），见 ctxPlaygroundBypass。

const playgroundApp = "playground"

// registerPlayground 挂载对话测试路由（管理会话鉴权与其它管理面接口一致）。
func (a *admin) registerPlayground(mux *http.ServeMux) {
	mux.Handle("GET /api/admin/playground/models", a.auth(a.playgroundModels))
	mux.Handle("POST /api/admin/playground/chat", a.auth(a.playgroundChat))
	mux.Handle("POST /api/admin/playground/audio/speech", a.auth(a.playgroundAudioSpeech))
	mux.Handle("POST /api/admin/playground/audio/transcriptions", a.auth(a.playgroundAudioTranscriptions))
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

// playgroundChat 把对话请求交给数据面：挑一把可用虚拟密钥，默认流式（TTS 等音频
// 协议走 audio 端点），响应（SSE 或错误 JSON）原样回传给管理页。
func (a *admin) playgroundChat(w http.ResponseWriter, r *http.Request) {
	if serverProxy == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "data plane not ready"})
		return
	}
	var in struct {
		Model      string           `json:"model"`
		Messages   []map[string]any `json:"messages"`
		MaxTokens  int64            `json:"max_tokens"`
		Stream     *bool            `json:"stream"`
		Modalities []string         `json:"modalities"`
		Audio      map[string]any   `json:"audio"`
	}
	// 附件以 base64 内联,上限放宽到与数据面一致(32MB)
	if readJSONLimit(w, r, &in, 32<<20) != nil {
		return
	}
	if in.Model == "" || len(in.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model and messages are required"})
		return
	}
	stream := in.Stream == nil || *in.Stream
	key := pickPlaygroundKey(a.st)
	if key == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "没有可用的启用虚拟密钥，请先在「虚拟密钥」页签发"})
		return
	}
	bodyMap := map[string]any{
		"model":    in.Model,
		"messages": in.Messages,
		"stream":   stream,
	}
	// TTS 等语音请求带 modalities/audio(响应在 message.audio.data);此时不设
	// max_tokens——音频输出按音频 token 计,设小了会截断语音。
	if len(in.Modalities) > 0 {
		bodyMap["modalities"] = in.Modalities
	}
	if in.Audio != nil {
		bodyMap["audio"] = in.Audio
	}
	if in.MaxTokens > 32768 {
		in.MaxTokens = 32768
	}
	if in.MaxTokens > 0 {
		bodyMap["max_tokens"] = in.MaxTokens
	} else if len(in.Modalities) == 0 {
		// 思考型模型的思维链会吃掉过小的 max_tokens 预算导致正文为空，默认给足
		bodyMap["max_tokens"] = 2048
	}
	body, err := json.Marshal(bodyMap)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req, err := http.NewRequestWithContext(playgroundCtx(r), http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-LiteGate-App", playgroundApp)
	serverProxy.serveOpenAI(w, req)
}

// playgroundAudioSpeech TTS 测试：标准 OpenAI audio/speech 协议 {model,input,voice}，
// 数据面桥接到 MiMo chat 语音协议，回传音频字节流（audio/wav）。
func (a *admin) playgroundAudioSpeech(w http.ResponseWriter, r *http.Request) {
	if serverProxy == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "data plane not ready"})
		return
	}
	var in struct {
		Model string `json:"model"`
		Input string `json:"input"`
		Voice string `json:"voice"`
	}
	if readJSONLimit(w, r, &in, 32<<20) != nil {
		return
	}
	if in.Model == "" || in.Input == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model and input are required"})
		return
	}
	key := pickPlaygroundKey(a.st)
	if key == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "没有可用的启用虚拟密钥，请先在「虚拟密钥」页签发"})
		return
	}
	body, err := json.Marshal(map[string]any{"model": in.Model, "input": in.Input, "voice": in.Voice})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req, err := http.NewRequestWithContext(playgroundCtx(r), http.MethodPost, "/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-LiteGate-App", playgroundApp)
	serverProxy.serveAudioSpeech(w, req)
}

// playgroundAudioTranscriptions ASR 测试：multipart {model,file} 原样转交数据面的
// audio/transcriptions 桥接，回传 {"text":...}。
func (a *admin) playgroundAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	if serverProxy == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "data plane not ready"})
		return
	}
	key := pickPlaygroundKey(a.st)
	if key == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "没有可用的启用虚拟密钥，请先在「虚拟密钥」页签发"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read multipart body"})
		return
	}
	req, err := http.NewRequestWithContext(playgroundCtx(r), http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-LiteGate-App", playgroundApp)
	serverProxy.serveAudioTranscriptions(w, req)
}

// pickPlaygroundKey 选一把启用虚拟密钥：优先不设白名单的。playground 请求带
// 白名单旁路标记，模型不受密钥白名单约束，这里只需任意一把启用密钥做鉴权/记账。
func pickPlaygroundKey(st *store.Store) string {
	keys, err := st.ListAPIKeys()
	if err != nil {
		return ""
	}
	for i := range keys {
		if keys[i].Enabled {
			return keys[i].Key
		}
	}
	return ""
}

// playgroundCtx 给 fabricated 请求挂上白名单旁路标记，并沿用原始请求的取消传播。
func playgroundCtx(r *http.Request) context.Context {
	return context.WithValue(r.Context(), ctxPlaygroundBypass{}, true)
}
