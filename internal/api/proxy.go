package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"litegate/internal/store"
)

const maxBodyBytes = 32 << 20

type proxy struct {
	st     *store.Store
	client *http.Client
	cache  modelsCache
	pc     priceCache
	keys   *keyHealthManager
	limits *keyAdmission
}

// priceCache 缓存价格表 60s，避免每笔请求都查一次库（SQLite 是单连接串行化）。
type priceCache struct {
	mu      sync.Mutex
	prices  []store.ModelPrice
	expires time.Time
}

func newUpstreamClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 300 * time.Second, // LLM 首字节可能很慢，不做整体超时
		},
	}
}

// upstreamClientForTest 供管理面连通性测试复用同一套 Transport 配置。
var upstreamClientForTest = newUpstreamClient()

func (p *proxy) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chat/completions", p.serveOpenAI)
	mux.HandleFunc("POST /v1/embeddings", p.serveOpenAI)
	mux.HandleFunc("POST /v1/messages", p.serveAnthropic)
	mux.HandleFunc("POST /v1/messages/count_tokens", p.serveCountTokens)
	mux.HandleFunc("POST /v1/responses", p.serveResponses)
	// 网关无状态，不存 Responses 对象：取回/删除走明确报错而不是落到管理页
	mux.HandleFunc("GET /v1/responses/{id}", p.serveResponsesStored)
	mux.HandleFunc("DELETE /v1/responses/{id}", p.serveResponsesStored)
	// 音频端点：TTS 请求是 JSON，转写是 multipart（model 在表单字段），都按 openai 渠道转发
	mux.HandleFunc("POST /v1/audio/speech", p.serveAudioSpeech)
	mux.HandleFunc("POST /v1/audio/transcriptions", p.serveAudioTranscriptions)
	// Groq SDK 风格客户端的路径自带 /openai/v1 前缀（base_url 只填主机名），注册同款别名
	mux.HandleFunc("POST /openai/v1/audio/speech", p.serveAudioSpeech)
	mux.HandleFunc("POST /openai/v1/audio/transcriptions", p.serveAudioTranscriptions)
	mux.HandleFunc("GET /v1/models", p.serveModels)
}

// serveOpenAI 把 /v1 下的资源路径原样映射到 openai 渠道（base_url 需含版本前缀）。
func (p *proxy) serveOpenAI(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, "openai", strings.TrimPrefix(r.URL.Path, "/v1"))
}

func (p *proxy) serveAnthropic(w http.ResponseWriter, r *http.Request) {
	p.serve(w, r, "anthropic", strings.TrimPrefix(r.URL.Path, "/v1"))
}

// serveAudioSpeech 桥接 OpenAI /v1/audio/speech（TTS）到 MiMo 的 chat.completions 语音协议：
// 小米不走标准 audio/speech 端点，而是 model=*-tts 的 chat 调用（文本放 assistant 消息、
// audio{format,voice} 参数），响应里 message.audio.data 是 base64 音频。
// 对外仍保持标准 OpenAI TTS 协议：客户端发 {model,input,voice}，收到音频字节流。
// MiMo 不支持语速参数（语速/情感走文本 style 标签），response_format 统一要 wav。
// 日志标 protocol=audio；TTS 按字符计费，无 token 用量，成本记 0。
func (p *proxy) serveAudioSpeech(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large or unreadable"})
		return
	}
	var in struct {
		Model string `json:"model"`
		Input string `json:"input"`
		Voice string `json:"voice"`
	}
	if err := json.Unmarshal(body, &in); err != nil || in.Model == "" || in.Input == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "audio/speech requires JSON body with model and input"})
		return
	}
	audio := map[string]any{"format": "wav"}
	if in.Voice != "" {
		audio["voice"] = in.Voice
	}
	chatBody, err := json.Marshal(map[string]any{
		"model":    in.Model,
		"messages": []map[string]any{{"role": "assistant", "content": in.Input}},
		"audio":    audio,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	p.serveBody(w, r, "openai", "/chat/completions", "audio", in.Model, chatBody, decodeChatAudio)
}

// decodeChatAudio 把 MiMo chat 语音响应（JSON，message.audio.data 为 base64）转换成
// audio/wav 字节流响应；上游非 200 时原样透传错误。
func decodeChatAudio(resp *http.Response) (*http.Response, error) {
	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var v struct {
		Choices []struct {
			Message struct {
				Audio struct {
					Data string `json:"data"`
				} `json:"audio"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &v); err != nil || len(v.Choices) == 0 || v.Choices[0].Message.Audio.Data == "" {
		return nil, fmt.Errorf("upstream chat response carries no audio data")
	}
	raw, err := base64.StdEncoding.DecodeString(v.Choices[0].Message.Audio.Data)
	if err != nil {
		return nil, fmt.Errorf("decode audio base64: %w", err)
	}
	header := http.Header{}
	header.Set("Content-Type", "audio/wav")
	return &http.Response{
		StatusCode: http.StatusOK,
		Proto:      "HTTP/1.1",
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(raw)),
	}, nil
}

// serveAudioTranscriptions 桥接 OpenAI /v1/audio/transcriptions（ASR）到 MiMo 的
// chat.completions 语音协议：小米的 ASR 也走 chat 调用（input_audio 内容块，且不接受
// text part，提示词由上游注入），响应文本在 message.content。
// 对外保持标准 OpenAI 协议：multipart {file,model} 进，{"text":...} 出。
func (p *proxy) serveAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := r.ParseMultipartForm(maxBodyBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "audio/transcriptions requires multipart/form-data"})
		return
	}
	defer r.MultipartForm.RemoveAll()
	model := strings.TrimSpace(r.FormValue("model"))
	if model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "audio/transcriptions requires a model field"})
		return
	}
	// OpenAI 协议的 response_format: json(默认)或 text；text 时直接回纯文本
	respFormat := strings.TrimSpace(r.FormValue("response_format"))
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "audio/transcriptions requires a file field"})
		return
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read audio file"})
		return
	}
	format := strings.TrimPrefix(strings.ToLower(path.Ext(hdr.Filename)), ".")
	if format == "" {
		format = "wav"
	}
	chatBody, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{{
				"type":        "input_audio",
				"input_audio": map[string]any{"data": base64.StdEncoding.EncodeToString(data), "format": format},
			}},
		}},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	p.serveBody(w, r, "openai", "/chat/completions", "audio", model, chatBody, func(resp *http.Response) (*http.Response, error) {
		return decodeChatText(resp, respFormat == "text")
	})
}

// decodeChatText 把 MiMo chat 转写响应（message.content 为文本）转换成标准
// OpenAI transcriptions 响应；wantText 时回纯文本（Groq 等客户端 response_format=text
// 的期望格式），否则回 {"text":...} JSON。上游非 200 时原样透传错误。
func decodeChatText(resp *http.Response, wantText bool) (*http.Response, error) {
	if resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	var v struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &v); err != nil || len(v.Choices) == 0 {
		return nil, fmt.Errorf("upstream chat response carries no transcription")
	}
	header := http.Header{}
	var out []byte
	if wantText {
		out = []byte(v.Choices[0].Message.Content)
		header.Set("Content-Type", "text/plain; charset=utf-8")
	} else {
		out, err = json.Marshal(map[string]string{"text": v.Choices[0].Message.Content})
		if err != nil {
			return nil, err
		}
		header.Set("Content-Type", "application/json")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Proto:      "HTTP/1.1",
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(out)),
	}, nil
}

// serve 是数据面主流程入口：读体、取模型名后进入 serveBody。
func (p *proxy) serve(w http.ResponseWriter, r *http.Request, protocol, upstreamPath string) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large or unreadable"})
		return
	}
	p.serveBody(w, r, protocol, upstreamPath, protocol, jsonModel(body), body, nil)
}

// ctxPlaygroundBypass 是管理台对话测试请求的上下文标记:跳过密钥模型白名单。
// 仅由进程内管理端点(playground.go)注入,外部请求无法携带。
type ctxPlaygroundBypass struct{}

// serveBody 是数据面主流程：鉴权 → 准入 → 选渠道 → 带故障转移地转发 → 回写响应并记日志。
// logProto 仅用于日志标注（音频类请求渠道仍按 openai 列，但日志记 audio 便于区分）。
// transform 非空时在回写前对上游响应做协议转换（音频桥接用）；转换失败按 502 记账。
func (p *proxy) serveBody(w http.ResponseWriter, r *http.Request, protocol, upstreamPath, logProto, model string, body []byte, transform func(*http.Response) (*http.Response, error)) {
	ak, err := p.authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid api key"})
		return
	}
	chans, err := p.st.ListChannels(protocol)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// 停用的渠道不接流量
	chans = enabledOnly(chans)
	chans = filterByModel(chans, model)
	if len(chans) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no enabled channel serves model: " + model})
		return
	}
	// 虚拟密钥的模型白名单：留空不限制；配置了则只放行列出的模型。
	// 管理台对话测试(fabricated 请求带 ctxPlaygroundBypass 标记)显式旁路白名单——
	// 所有启用模型均可测试；限流/预算/日志照常生效。
	if model != "" && !ak.AllowsModel(model) && r.Context().Value(ctxPlaygroundBypass{}) == nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "model not allowed for this api key: " + model})
		return
	}
	// 准入控制：过期 / RPM/TPM 限速 / 日月预算
	if code, msg := p.limits.admit(ak); code != 0 {
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "60")
		}
		writeJSON(w, code, map[string]string{"error": msg})
		return
	}

	start := time.Now()
	app := inboundApp(r)
	// openai 流式请求补 stream_options.include_usage 以获取 usage；上游不识别时回退重试
	ab := &attemptBodies{current: body, plain: body}
	if protocol == "openai" {
		if b, ok := injectStreamUsage(body); ok {
			ab.current = b
			ab.injected = true
		}
	}
	resp, c, _, lastErr := p.dispatch(r, chans, upstreamPath, ab, model)
	if resp == nil {
		// 对下游只给通用错误：渠道名/上游地址等细节留给服务端日志，防止虚拟密钥持有者探测内部拓扑
		if lastErr != nil {
			log.Printf("proxy %s %s failed: %v", r.Method, upstreamPath, lastErr)
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "all channels failed"})
		p.logRequest(ak, nil, logProto, model, http.StatusBadGateway, time.Since(start), time.Since(start), tokenUsage{}, errMsg(lastErr), app)
		return
	}
	if transform != nil {
		tr, terr := transform(resp)
		if terr != nil {
			resp.Body.Close()
			log.Printf("proxy %s %s transform failed: %v", r.Method, upstreamPath, terr)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "upstream response transform failed"})
			p.logRequest(ak, c, logProto, model, http.StatusBadGateway, time.Since(start), time.Since(start), tokenUsage{}, errMsg(terr), app)
			return
		}
		resp = tr
	}
	p.respond(w, ak, c, logProto, model, resp, start, app)
}

// inboundApp 提取应用归因头：客户端可选自带 X-LiteGate-App 标记调用方，
// 日志与用量按它分摊。截断到 64 字节防滥用。
func inboundApp(r *http.Request) string {
	app := strings.TrimSpace(r.Header.Get("X-LiteGate-App"))
	if len(app) > 64 {
		app = app[:64]
	}
	return app
}

const maxProxyAttempts = 4

// attemptBodies 是尝试过程中的请求体：current 可能带注入的 stream_options，
// plain 为原始体；上游 400 不识别时回退 plain 重试。
type attemptBodies struct {
	current  []byte
	plain    []byte
	injected bool
}

// dispatch 是数据面统一的转发决策：渠道候选按优先级/权重排序依次尝试；渠道内
// 在健康密钥间轮换，失败即上报冷却。规则：
//   - 多 key 渠道把 401/403 视为密钥问题换 key 重试；单 key 保持透传（配置错误应可见）
//   - 其余可重试状态（408/429/5xx）按既有语义故障转移
//   - 总尝试次数封顶 maxProxyAttempts，避免长链拖高延迟
//
// resp == nil 表示全部尝试失败；lastErr 只进服务端日志。
func (p *proxy) dispatch(r *http.Request, chans []store.Channel, path string, ab *attemptBodies, model string) (*http.Response, *store.Channel, *store.ChannelKey, error) {
	attempts := orderCandidates(chans)
	if len(attempts) > maxProxyAttempts {
		attempts = attempts[:maxProxyAttempts]
	}
	var lastErr error
	used := 0
	for i := range attempts {
		c := &attempts[i]
		keys := p.keys.available(c, time.Now())
		if len(keys) == 0 {
			continue
		}
		enabledKeys := 0
		for _, k := range c.APIKeys {
			if k.Enabled {
				enabledKeys++
			}
		}
		for ki := range keys {
			k := &keys[ki]
			if used >= maxProxyAttempts {
				return nil, nil, nil, lastErr
			}
			used++
			resp, err := p.attemptUpstream(r, c, k.Key, path, rewriteModel(ab.current, c, model))
			if err != nil {
				lastErr = fmt.Errorf("channel %q: %w", c.Name, err)
				p.keys.reportFailure(c.ID, k.ID)
				continue
			}
			if ab.injected && resp.StatusCode == http.StatusBadRequest {
				// 上游不识别 stream_options.include_usage：去掉该字段对同渠道同密钥重试一次
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				ab.current, ab.injected = ab.plain, false
				resp, err = p.attemptUpstream(r, c, k.Key, path, rewriteModel(ab.current, c, model))
				if err != nil {
					lastErr = fmt.Errorf("channel %q: %w", c.Name, err)
					p.keys.reportFailure(c.ID, k.ID)
					continue
				}
			}
			spare := ki < len(keys)-1 || i < len(attempts)-1
			if shouldRotate(resp.StatusCode, enabledKeys > 1) && spare {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				lastErr = fmt.Errorf("channel %q: upstream status %d", c.Name, resp.StatusCode)
				p.keys.reportFailure(c.ID, k.ID)
				continue
			}
			p.keys.reportSuccess(c.ID, k.ID)
			return resp, c, k, nil
		}
	}
	return nil, nil, nil, lastErr
}

// shouldRotate 决定响应是否应换 key/渠道：多 key 时 401/403 也视为密钥问题；
// 单 key 保持原语义，401/403/400 作为确定性失败透传。
func shouldRotate(code int, multiKey bool) bool {
	if multiKey && (code == http.StatusUnauthorized || code == http.StatusForbidden) {
		return true
	}
	return isRetryableStatus(code)
}

// rewriteModel 按渠道的模型映射把对外模型名改写为上游真实名；无映射原样透传。
func rewriteModel(body []byte, c *store.Channel, model string) []byte {
	if model == "" || len(c.ModelMap) == 0 {
		return body
	}
	real := c.UpstreamModel(model)
	if real == model {
		return body
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v map[string]any
	if dec.Decode(&v) != nil || v == nil {
		return body
	}
	v["model"] = real
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return out
}

// errMsg 返回内部错误的安全摘要（截断，供日志表使用）。
func errMsg(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

// enabledOnly 过滤掉停用的渠道。
func enabledOnly(chans []store.Channel) []store.Channel {
	out := chans[:0:0]
	for _, c := range chans {
		if c.Enabled {
			out = append(out, c)
		}
	}
	return out
}

func (p *proxy) attemptUpstream(r *http.Request, c *store.Channel, key, path string, body []byte) (*http.Response, error) {
	upstream := c.BaseURL + path
	if r.URL.RawQuery != "" {
		// 客户端查询串原样透传（如 Azure 的 api-version、beta 开关）
		upstream += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	setUpstreamHeaders(req, r, c, key)
	return p.client.Do(req)
}

// respond 把上游响应回写给客户端；进入此函数后不再故障转移。
// 同时被动提取 usage：流式靠 sseUsageScanner 逐行嗅探，非流式保留响应体
// 末尾 64KB（usage 位于 JSON 尾部）等复制完成后再解析。
func (p *proxy) respond(w http.ResponseWriter, ak *store.APIKey, c *store.Channel, protocol, model string, resp *http.Response, start time.Time, app string) {
	defer resp.Body.Close()
	ttfb := time.Since(start) // 上游返回响应头的耗时，近似上游首包延迟
	ct := resp.Header.Get("Content-Type")
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)

	var u tokenUsage
	var err error
	if strings.HasPrefix(ct, "text/event-stream") {
		var scan sseUsageScanner
		_, err = streamCopy(w, resp.Body, &scan)
		u = scan.usage
	} else {
		tail := &tailBuffer{cap: maxUsageTail}
		_, err = io.Copy(w, io.TeeReader(resp.Body, tail))
		u, _ = usageFromJSON(tail.bytes())
	}
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	p.logRequest(ak, c, protocol, model, resp.StatusCode, time.Since(start), ttfb, u, errMsg, app)
}

// authenticate 校验下游虚拟密钥：Authorization: Bearer 或 X-Api-Key（Claude Code 风格）。
func (p *proxy) authenticate(r *http.Request) (*store.APIKey, error) {
	h := r.Header.Get("Authorization")
	key := ""
	if strings.HasPrefix(h, "Bearer ") {
		key = strings.TrimSpace(h[len("Bearer "):])
	} else {
		key = r.Header.Get("X-Api-Key")
	}
	if key == "" {
		return nil, errors.New("missing api key")
	}
	return p.st.LookupAPIKey(key)
}

// passthroughHeaders 需要原样转发给上游的功能性请求头（白名单制，防止客户端伪造计费/身份头）。
var passthroughHeaders = []string{"Anthropic-Beta", "OpenAI-Beta", "X-Stainless-Lang", "X-Stainless-Package-Version"}

func setUpstreamHeaders(req *http.Request, inbound *http.Request, c *store.Channel, key string) {
	ct := inbound.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Accept", inbound.Header.Get("Accept"))
	for _, h := range passthroughHeaders {
		if v := inbound.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	switch c.Type {
	case "anthropic":
		req.Header.Set("X-Api-Key", key)
		v := inbound.Header.Get("Anthropic-Version")
		if v == "" {
			v = "2023-06-01"
		}
		req.Header.Set("Anthropic-Version", v)
	default:
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

func jsonModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	// model 缺失时返回空串，由“无可用渠道”逻辑兜底
	_ = json.Unmarshal(body, &v)
	return v.Model
}

// filterByModel 保留当前服务该模型的渠道（显式列表 + 通配渠道的禁用列表，见 ServesModel）。
func filterByModel(chans []store.Channel, model string) []store.Channel {
	if model == "" {
		return chans
	}
	out := chans[:0:0]
	for _, c := range chans {
		if c.ServesModel(model) {
			out = append(out, c)
		}
	}
	return out
}

// orderCandidates 最高优先级组内做无放回加权随机，其余渠道按优先级兜底。
func orderCandidates(chans []store.Channel) []store.Channel {
	maxPrio := chans[0].Priority
	for _, c := range chans {
		if c.Priority > maxPrio {
			maxPrio = c.Priority
		}
	}
	var top, rest []store.Channel
	for _, c := range chans {
		if c.Priority == maxPrio {
			top = append(top, c)
		} else {
			rest = append(rest, c)
		}
	}
	return append(weightedOrder(top), rest...)
}

func weightedOrder(items []store.Channel) []store.Channel {
	pool := append([]store.Channel(nil), items...)
	out := make([]store.Channel, 0, len(pool))
	for len(pool) > 0 {
		total := 0
		for _, c := range pool {
			total += max(c.Weight, 1)
		}
		x := rand.IntN(total)
		idx := 0
		for i, c := range pool {
			x -= max(c.Weight, 1)
			if x < 0 {
				idx = i
				break
			}
		}
		out = append(out, pool[idx])
		pool = append(pool[:idx], pool[idx+1:]...)
	}
	return out
}

// isRetryableStatus 判定可故障转移的状态码；401/400/404 等视为确定性失败直接透传。
func isRetryableStatus(code int) bool {
	return code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500
}

// streamCopy 边读边写边 Flush，保证 SSE 首字节延迟与断流传播；
// scan 非空时把透传的字节喂给 usage 嗅探器。
func streamCopy(w http.ResponseWriter, src io.Reader, scan *sseUsageScanner) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return total, werr
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if scan != nil {
				_, _ = scan.Write(buf[:n])
			}
			total += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
	}
}

// 尾部环形缓冲：非流式响应只保留最后 cap 字节用于解析 usage，
// 内存有上界，不随响应体大小增长。
const maxUsageTail = 64 << 10

type tailBuffer struct {
	buf []byte
	cap int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.cap {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.cap:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) bytes() []byte { return t.buf }

func (p *proxy) logRequest(ak *store.APIKey, c *store.Channel, protocol, model string, status int, total, ttfb time.Duration, u tokenUsage, errMsg string, app string) {
	if len(errMsg) > 512 {
		errMsg = errMsg[:512]
	}
	if len(app) > 64 {
		app = app[:64]
	}
	var price *store.ModelPrice
	if u.prompt > 0 || u.completion > 0 {
		price = p.lookupPrice(model)
	}
	cost := store.CostOf(price, time.Now(), u.prompt, u.cacheRead, u.cacheWrite, u.completion)
	p.limits.record(ak, u.prompt, u.completion, cost)
	l := &store.RequestLog{
		Model: model, Protocol: protocol, App: app, Status: status,
		LatencyMs: total.Milliseconds(), TtfbMs: ttfb.Milliseconds(),
		PromptTokens: u.prompt, CompletionTokens: u.completion, CacheTokens: u.cacheRead,
		CostUSD: cost,
		Error:   errMsg,
	}
	if ak != nil {
		l.APIKeyID = ak.ID
	}
	if c != nil {
		l.ChannelID = c.ID
	}
	if err := p.st.InsertRequestLog(l); err != nil {
		log.Printf("insert request log: %v", err)
	}
}

// lookupPrice 带 60s 缓存的价格匹配，规则见 matchPrice；查不到返回 nil（成本记 0）。
func (p *proxy) lookupPrice(model string) *store.ModelPrice {
	p.pc.mu.Lock()
	defer p.pc.mu.Unlock()
	if time.Now().After(p.pc.expires) {
		if prices, err := p.st.ListModelPrices(); err == nil {
			p.pc.prices = prices
			p.pc.expires = time.Now().Add(60 * time.Second)
		}
	}
	return matchPrice(p.pc.prices, model)
}
