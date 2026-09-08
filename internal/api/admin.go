package api

import (
	"crypto/subtle"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"litegate/internal/cryptoutil"
	"litegate/internal/store"
)

type admin struct {
	st       *store.Store
	password string
	// invalidateModels/invalidatePrices 通知数据面失效对应缓存
	// （渠道或模型变更、价格变更时调用，漏调会导致最长 60 秒脏数据）。
	invalidateModels func()
	invalidatePrices func()

	mu       sync.Mutex
	sessions map[string]time.Time
	failures map[string]int     // 来源 IP → 连续登录失败次数
	locked   map[string]time.Time // 来源 IP → 锁定截止时间
}

func (a *admin) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/admin/login", a.login)
	mux.Handle("GET /api/admin/dashboard", a.auth(a.dashboard))
	mux.Handle("GET /api/admin/channels", a.auth(a.listChannels))
	mux.Handle("POST /api/admin/channels", a.auth(a.createChannel))
	mux.Handle("PUT /api/admin/channels/{id}", a.auth(a.updateChannel))
	mux.Handle("DELETE /api/admin/channels/{id}", a.auth(a.deleteChannel))
	mux.Handle("POST /api/admin/channels/{id}/test", a.auth(a.testChannel))
	mux.Handle("POST /api/admin/channels/{id}/keys/{key_id}/enable", a.auth(a.enableChannelKey))
	mux.Handle("POST /api/admin/channels/{id}/keys/{key_id}/disable", a.auth(a.disableChannelKey))
	mux.Handle("POST /api/admin/channels/{id}/models/disable/{model...}", a.auth(a.disableChannelModel))
	mux.Handle("POST /api/admin/channels/{id}/models/enable/{model...}", a.auth(a.enableChannelModel))
	mux.Handle("GET /api/admin/keys", a.auth(a.listKeys))
	mux.Handle("POST /api/admin/keys", a.auth(a.createKey))
	mux.Handle("PUT /api/admin/keys/{id}", a.auth(a.updateKey))
	mux.Handle("GET /api/admin/keys/{id}/reveal", a.auth(a.revealKey))
	mux.Handle("DELETE /api/admin/keys/{id}", a.auth(a.deleteKey))
	mux.Handle("GET /api/admin/channels/{id}/discover", a.auth(a.discoverChannelModels))
	mux.Handle("POST /api/admin/db/backup", a.auth(a.backupDB))
	mux.Handle("GET /api/admin/db/backups", a.auth(a.listBackups))
	mux.Handle("GET /api/admin/config/export", a.auth(a.exportConfig))
	mux.Handle("POST /api/admin/config/import", a.auth(a.importConfig))
	mux.Handle("GET /api/admin/logs", a.auth(a.listLogs))
	mux.Handle("GET /api/admin/prices", a.auth(a.listPrices))
	mux.Handle("PUT /api/admin/prices", a.auth(a.upsertPrice))
	mux.Handle("DELETE /api/admin/prices/{model...}", a.auth(a.deletePrice))
}

func (a *admin) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		a.mu.Lock()
		exp, ok := a.sessions[tok]
		a.mu.Unlock()
		if tok == "" || !ok || time.Now().After(exp) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	})
}

func (a *admin) login(w http.ResponseWriter, r *http.Request) {
	// 登录限速：同一来源 IP 连续失败 5 次锁定 60 秒（内存态，重启即清零）
	ip := clientIP(r)
	a.mu.Lock()
	if a.locked != nil {
		if until, ok := a.locked[ip]; ok {
			if time.Now().Before(until) {
				a.mu.Unlock()
				writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many failed attempts, try later"})
				return
			}
			delete(a.locked, ip)
		}
	}
	a.mu.Unlock()

	var req struct {
		Password string `json:"password"`
	}
	if readJSON(w, r, &req) != nil {
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Password), []byte(a.password)) != 1 {
		a.mu.Lock()
		if a.locked == nil {
			a.locked = map[string]time.Time{}
		}
		a.failures[ip]++
		if a.failures[ip] >= 5 {
			a.locked[ip] = time.Now().Add(60 * time.Second)
			delete(a.failures, ip)
		}
		a.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong password"})
		return
	}
	tok := cryptoutil.RandomHex(32)
	a.mu.Lock()
	a.sessions[tok] = time.Now().Add(7 * 24 * time.Hour)
	delete(a.failures, ip)
	for k, exp := range a.sessions { // 顺手清理过期会话
		if time.Now().After(exp) {
			delete(a.sessions, k)
		}
	}
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

// clientIP 提取来源 IP（家庭内网直接 RemoteAddr；经 nginx 时 X-Real-IP 可信）。
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (a *admin) dashboard(w http.ResponseWriter, _ *http.Request) {
	d, err := a.st.Dashboard()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// ---- 渠道管理 ----

type channelIn struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	BaseURL string   `json:"base_url"`
	// APIKeys 是渠道的上游密钥列表（明文），保存时加密。create 必填（可空串）；
	// update 传 nil 表示密钥不动，传数组（含空数组）表示全量替换（启停状态按密钥保留）。
	APIKeys []string `json:"api_keys"`
	// APIKey 是旧版单密钥字段的兼容入口：仅当 api_keys 未传且 api_key 非空时生效。
	APIKey  string            `json:"api_key"`
	Models  []string          `json:"models"`
	// DisabledModels 可选：显式指定禁用列表（批量导入场景全量替换）。
	// 不传（nil）时按"移出启用列表即禁用"的规则自动推导。
	DisabledModels *[]string        `json:"disabled_models"`
	ModelMap       map[string]string `json:"model_map"` // 模型映射：对外名 → 上游真实名
	Weight         int               `json:"weight"`
	Priority       int               `json:"priority"`
	Enabled        *bool             `json:"enabled"`
	Remark         string            `json:"remark"`
}

func (in *channelIn) validate() string {
	if in.Name == "" {
		return "name is required"
	}
	if in.Type != "openai" && in.Type != "anthropic" {
		return "type must be openai or anthropic"
	}
	if in.BaseURL == "" {
		return "base_url is required"
	}
	return ""
}

// keyInputs 把入参归一为密钥明文列表：兼容旧版单 api_key 字段。
func (in *channelIn) keyInputs() []string {
	if len(in.APIKeys) > 0 {
		return in.APIKeys
	}
	if in.APIKey != "" {
		return []string{in.APIKey}
	}
	if in.APIKeys != nil {
		return []string{}
	}
	return nil
}

// channelOut 是渠道的对外视图：密钥只回打码值，避免明文回显。
type channelOut struct {
	ID             int64               `json:"id"`
	Name           string              `json:"name"`
	Type           string              `json:"type"`
	BaseURL        string              `json:"base_url"`
	APIKeys        []store.ChannelKey  `json:"api_keys"`
	Models         []string            `json:"models"`
	DisabledModels []string            `json:"disabled_models"`
	ModelMap       map[string]string   `json:"model_map"`
	Weight         int                 `json:"weight"`
	Priority       int                 `json:"priority"`
	Enabled        bool                `json:"enabled"`
	Remark         string              `json:"remark"`
	CreatedAt      string              `json:"created_at"`
}

func maskChannel(c *store.Channel) channelOut {
	keys := make([]store.ChannelKey, 0, len(c.APIKeys))
	for _, k := range c.APIKeys {
		keys = append(keys, store.ChannelKey{ID: k.ID, Masked: store.MaskKey(k.Key), Enabled: k.Enabled})
	}
	return channelOut{
		ID: c.ID, Name: c.Name, Type: c.Type, BaseURL: c.BaseURL,
		APIKeys: keys, Models: c.Models, DisabledModels: c.DisabledModels,
		ModelMap: c.ModelMap, Weight: c.Weight, Priority: c.Priority, Enabled: c.Enabled,
		Remark: c.Remark, CreatedAt: c.CreatedAt,
	}
}

func (a *admin) listChannels(w http.ResponseWriter, _ *http.Request) {
	chans, err := a.st.ListChannels("")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]channelOut, 0, len(chans))
	for i := range chans {
		out = append(out, maskChannel(&chans[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *admin) createChannel(w http.ResponseWriter, r *http.Request) {
	var in channelIn
	if readJSON(w, r, &in) != nil {
		return
	}
	if msg := in.validate(); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	var disabled []string
	if in.DisabledModels != nil {
		disabled = *in.DisabledModels
	}
	var keys []store.ChannelKey
	for _, k := range in.keyInputs() {
		keys = append(keys, store.ChannelKey{Key: k})
	}
	id, err := a.st.CreateChannel(&store.Channel{
		Name: in.Name, Type: in.Type, BaseURL: in.BaseURL, APIKeys: keys,
		Models: in.Models, DisabledModels: disabled, ModelMap: in.ModelMap,
		Weight: in.Weight, Priority: in.Priority,
		Enabled: enabled, Remark: in.Remark,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.invalidateModels()
	writeJSON(w, http.StatusOK, map[string]int64{"id": id})
}

func (a *admin) updateChannel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	old, err := a.st.GetChannel(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}
	var in channelIn
	if readJSON(w, r, &in) != nil {
		return
	}
	if msg := in.validate(); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	// 密钥：入参未传 api_keys（且未用旧版 api_key）表示沿用原密钥；传了则全量替换
	//（替换时存储层按明文保留各密钥的启停状态）。
	inKeys := in.keyInputs()
	var newKeys []store.ChannelKey
	if inKeys != nil {
		for _, k := range inKeys {
			newKeys = append(newKeys, store.ChannelKey{Key: k})
		}
	} else {
		newKeys = old.APIKeys
	}
	enabled := old.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	// 模型列表全量替换：移出启用列表的模型自动转入禁用列表（重新添加即恢复），
	// 这样不会丢失"曾经配置过"的信息，也符合"禁用后重新添加即启用"的操作习惯。
	// 调用方显式传入 disabled_models 时以其为准（批量导入场景）。
	var newDisabled []string
	if in.DisabledModels != nil {
		newDisabled = *in.DisabledModels
	} else {
		disabled := old.DisabledModels
		for _, m := range old.Models {
			if !containsStr(in.Models, m) && !containsStr(disabled, m) {
				disabled = append(disabled, m)
			}
		}
		newDisabled = disabled[:0:0]
		for _, m := range disabled {
			if !containsStr(in.Models, m) {
				newDisabled = append(newDisabled, m)
			}
		}
	}
	err = a.st.UpdateChannel(&store.Channel{
		ID: id, Name: in.Name, Type: in.Type, BaseURL: in.BaseURL, APIKeys: newKeys,
		Models: in.Models, DisabledModels: newDisabled, ModelMap: in.ModelMap,
		Weight: in.Weight, Priority: in.Priority,
		Enabled: enabled, Remark: in.Remark, CreatedAt: old.CreatedAt,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.invalidateModels()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// containsStr 是 containsModel 的 api 包本地别名（store 里的同名助手未导出）。
func containsStr(list []string, s string) bool {
	for _, m := range list {
		if m == s {
			return true
		}
	}
	return false
}

// disableChannelModel 禁用渠道上的单个模型：POST .../models/disable/{model...}
func (a *admin) disableChannelModel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	model := r.PathValue("model")
	if model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	if err := a.st.DisableModel(id, model); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}
	a.invalidateModels()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// enableChannelModel 重新启用渠道上的单个模型：POST .../models/enable/{model...}
func (a *admin) enableChannelModel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	model := r.PathValue("model")
	if model == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model is required"})
		return
	}
	if err := a.st.EnableModel(id, model); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}
	a.invalidateModels()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *admin) deleteChannel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.st.DeleteChannel(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}
	a.invalidateModels()
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *admin) testChannel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c, err := a.st.GetChannel(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}
	ctx := r.Context()
	type keyResult struct {
		Masked string `json:"masked"`
		OK     bool   `json:"ok"`
		Models int    `json:"models,omitempty"`
		Error  string `json:"error,omitempty"`
	}
	keys := c.APIKeys
	results := make([]keyResult, 0, len(keys))
	okAll := len(keys) > 0
	for _, k := range keys {
		res := keyResult{Masked: store.MaskKey(k.Key)}
		n, err := fetchFromChannel(ctx, upstreamClientForTest, c, k.Key)
		if err != nil {
			res.Error = err.Error()
			okAll = false
		} else {
			res.OK = true
			res.Models = len(n)
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": okAll, "keys": results})
}

// enableChannelKey / disableChannelKey 手动启停渠道上的单把密钥。
func (a *admin) enableChannelKey(w http.ResponseWriter, r *http.Request) {
	a.setChannelKey(w, r, true)
}

func (a *admin) disableChannelKey(w http.ResponseWriter, r *http.Request) {
	a.setChannelKey(w, r, false)
}

func (a *admin) setChannelKey(w http.ResponseWriter, r *http.Request, enabled bool) {
	keyID, _ := strconv.ParseInt(r.PathValue("key_id"), 10, 64)
	if err := a.st.SetChannelKeyEnabled(keyID, enabled); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel key not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---- 虚拟密钥管理 ----

// maskApiKey 打码虚拟密钥：保留前缀与末 4 位，中间以 **** 代替。
func maskApiKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 12 {
		return strings.Repeat("*", len(k))
	}
	return k[:6] + "****" + k[len(k)-4:]
}

type apiKeyOut struct {
	ID            int64    `json:"id"`
	Key           string   `json:"key"` // 打码后的密钥，明文需经 reveal 端点按需获取
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models"`
	Enabled       bool     `json:"enabled"`
	CreatedAt     string   `json:"created_at"`
	ExpiresAt     string   `json:"expires_at"`
	RPMLimit      int64    `json:"rpm_limit"`
	TPMLimit      int64    `json:"tpm_limit"`
	BudgetUSD     float64  `json:"budget_usd"`
	BudgetPeriod  string   `json:"budget_period"`
	BudgetTokens  int64    `json:"budget_tokens"`
	// 近 7 天输出速度统计（成功且可计算的请求）
	AvgTps       float64 `json:"avg_tps"`
	RecentReqs   int64   `json:"recent_requests"`
}

func (a *admin) listKeys(w http.ResponseWriter, _ *http.Request) {
	keys, err := a.st.ListAPIKeys()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	speeds, err := a.st.KeySpeedStats()
	if err != nil {
		speeds = map[int64]store.KeySpeedStat{} // 统计失败不影响列表主体
	}
	out := make([]apiKeyOut, 0, len(keys))
	for i := range keys {
		k := &keys[i]
		var tps float64
		var recent int64
		if st, ok := speeds[k.ID]; ok {
			tps = math.Round(st.Tps()*10) / 10
			recent = st.Requests
		}
		out = append(out, apiKeyOut{
			ID: k.ID, Key: maskApiKey(k.Key), Name: k.Name,
			AllowedModels: k.AllowedModels, Enabled: k.Enabled, CreatedAt: k.CreatedAt,
			ExpiresAt: k.ExpiresAt, RPMLimit: k.RPMLimit, TPMLimit: k.TPMLimit,
			BudgetUSD: k.BudgetUSD, BudgetPeriod: k.BudgetPeriod, BudgetTokens: k.BudgetTokens,
			AvgTps: tps, RecentReqs: recent,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// revealKey 返回密钥明文。列表接口默认只给打码值，明文按需获取，避免管理页一屏明文。
func (a *admin) revealKey(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	k, err := a.st.GetAPIKey(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": k.Key})
}

// keyLimitsIn 是虚拟密钥的治理字段（过期 / 限速 / 预算）；0/空 = 不限制。
type keyLimitsIn struct {
	ExpiresAt    string  `json:"expires_at"` // YYYY-MM-DD，当日结束失效
	RPMLimit     int64   `json:"rpm_limit"`
	TPMLimit     int64   `json:"tpm_limit"`
	BudgetUSD    float64 `json:"budget_usd"`
	BudgetPeriod string  `json:"budget_period"` // daily | monthly
	BudgetTokens int64   `json:"budget_tokens"`
}

func (a *admin) createKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name          string   `json:"name"`
		AllowedModels []string `json:"allowed_models"`
		keyLimitsIn
	}
	if readJSON(w, r, &req) != nil {
		return
	}
	k := &store.APIKey{
		Name: req.Name, AllowedModels: req.AllowedModels, Enabled: true,
		ExpiresAt: req.ExpiresAt, RPMLimit: req.RPMLimit, TPMLimit: req.TPMLimit,
		BudgetUSD: req.BudgetUSD, BudgetPeriod: req.BudgetPeriod, BudgetTokens: req.BudgetTokens,
	}
	if err := a.st.CreateAPIKey(k); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, k)
}

// updateKey 更新密钥的名称、模型限制与治理字段（全量替换；allowed_models 留空 = 不限制）。
func (a *admin) updateKey(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var req struct {
		Name          string   `json:"name"`
		AllowedModels []string `json:"allowed_models"`
		keyLimitsIn
	}
	if readJSON(w, r, &req) != nil {
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	k := &store.APIKey{
		ID: id, Name: req.Name, AllowedModels: req.AllowedModels,
		ExpiresAt: req.ExpiresAt, RPMLimit: req.RPMLimit, TPMLimit: req.TPMLimit,
		BudgetUSD: req.BudgetUSD, BudgetPeriod: req.BudgetPeriod, BudgetTokens: req.BudgetTokens,
	}
	if err := a.st.UpdateAPIKey(k); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *admin) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := a.st.DeleteAPIKey(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "key not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *admin) listLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	f := store.LogFilter{
		Limit:  limit,
		Offset: offset,
		Model:  q.Get("model"),
		App:    q.Get("app"),
		Status: q.Get("status"),
		Since:  q.Get("since"),
		Until:  q.Get("until"),
	}
	f.ChannelID, _ = strconv.ParseInt(q.Get("channel_id"), 10, 64)
	f.APIKeyID, _ = strconv.ParseInt(q.Get("api_key_id"), 10, 64)
	page, err := a.st.ListLogs(f)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// ---- 模型价格管理 ----

// priceIn 的 currency 为可选标注字段（"USD"|"CNY"），缺省 USD；不做汇率换算。
type priceIn struct {
	Model       string  `json:"model"`
	InputPrice  float64 `json:"input_price"`
	OutputPrice float64 `json:"output_price"`
	Currency    string  `json:"currency"`
}

var priceCurrencies = map[string]bool{"USD": true, "CNY": true}

func (a *admin) listPrices(w http.ResponseWriter, _ *http.Request) {
	prices, err := a.st.ListModelPrices()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, prices)
}

func (a *admin) upsertPrice(w http.ResponseWriter, r *http.Request) {
	var in priceIn
	if readJSON(w, r, &in) != nil {
		return
	}
	in.Model = strings.TrimSpace(in.Model)
	in.Currency = strings.ToUpper(strings.TrimSpace(in.Currency))
	if in.Currency == "" {
		in.Currency = "USD"
	}
	if in.Model == "" || in.InputPrice < 0 || in.OutputPrice < 0 || !priceCurrencies[in.Currency] {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "model is required, prices must be non-negative and currency must be USD or CNY"})
		return
	}
	p := &store.ModelPrice{Model: in.Model, InputPrice: in.InputPrice, OutputPrice: in.OutputPrice, Currency: in.Currency}
	if err := a.st.UpsertModelPrice(p); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if a.invalidatePrices != nil {
		a.invalidatePrices()
	}
	writeJSON(w, http.StatusOK, p)
}

func (a *admin) deletePrice(w http.ResponseWriter, r *http.Request) {
	model := r.PathValue("model")
	if err := a.st.DeleteModelPrice(model); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "price not found"})
		return
	}
	if a.invalidatePrices != nil {
		a.invalidatePrices()
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
