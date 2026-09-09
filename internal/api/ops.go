package api

// M5 运维与管理面扩展：SQLite 备份、配置导入导出、渠道模型自动发现。

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"litegate/internal/store"
)

// ---- SQLite 备份 ----

// backupDB 在线备份：POST /api/admin/db/backup → {file,size,created}
func (a *admin) backupDB(w http.ResponseWriter, _ *http.Request) {
	info, err := a.st.Backup()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// listBackups 备份列表：GET /api/admin/db/backups → [{file,size,created}]
func (a *admin) listBackups(w http.ResponseWriter, _ *http.Request) {
	list, err := a.st.ListBackups()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// ---- 配置导出 / 导入 ----

// exportedChannel 是配置文件里的渠道形态；APIKeys 明文仅在导出带 include_keys=1 时填充。
type exportedChannel struct {
	Name           string            `json:"name"`
	Type           string            `json:"type"`
	BaseURL        string            `json:"base_url"`
	APIKeys        []string          `json:"api_keys,omitempty"`
	KeysMasked     []string          `json:"keys_masked,omitempty"`
	Models         []string          `json:"models"`
	DisabledModels []string          `json:"disabled_models"`
	ModelMap       map[string]string `json:"model_map"`
	Weight         int               `json:"weight"`
	Priority       int               `json:"priority"`
	Enabled        bool              `json:"enabled"`
	Remark         string            `json:"remark"`
}

type configExport struct {
	ExportedAt string            `json:"exported_at"`
	Channels   []exportedChannel `json:"channels"`
	Prices     []store.ModelPrice `json:"prices"`
}

// exportConfig 导出渠道与价格配置：GET /api/admin/config/export?include_keys=1。
// 默认不导出明文密钥（只给打码值，导入时按名字沿用库里已有密钥）；
// include_keys=1 导出明文——产出文件等同凭证，务必妥善保管。
func (a *admin) exportConfig(w http.ResponseWriter, r *http.Request) {
	includeKeys := r.URL.Query().Get("include_keys") == "1"
	chans, err := a.st.ListChannels("")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	prices, err := a.st.ListModelPrices()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := configExport{ExportedAt: time.Now().UTC().Format("2006-01-02 15:04:05"),
		Channels: []exportedChannel{}, Prices: prices}
	for _, c := range chans {
		ec := exportedChannel{
			Name: c.Name, Type: c.Type, BaseURL: c.BaseURL,
			Models: c.Models, DisabledModels: c.DisabledModels, ModelMap: c.ModelMap,
			Weight: c.Weight, Priority: c.Priority, Enabled: c.Enabled, Remark: c.Remark,
		}
		for _, k := range c.APIKeys {
			ec.KeysMasked = append(ec.KeysMasked, store.MaskKey(k.Key))
			if includeKeys && k.Enabled {
				ec.APIKeys = append(ec.APIKeys, k.Key)
			}
		}
		out.Channels = append(out.Channels, ec)
	}
	writeJSON(w, http.StatusOK, out)
}

// importConfig 导入渠道与价格：POST /api/admin/config/import。
// 渠道按 name 匹配：存在则更新（api_keys 未给 = 沿用原密钥），不存在则新建；
// 价格逐条 upsert。
func (a *admin) importConfig(w http.ResponseWriter, r *http.Request) {
	var in configExport
	if readJSON(w, r, &in) != nil {
		return
	}
	imported := 0
	var errs []string
	for _, ec := range in.Channels {
		if ec.Name == "" {
			errs = append(errs, "跳过 name 为空的渠道")
			continue
		}
		existing, err := a.st.GetChannelByName(ec.Name)
		var keys []store.ChannelKey
		if len(ec.APIKeys) > 0 {
			for _, k := range ec.APIKeys {
				keys = append(keys, store.ChannelKey{Key: k})
			}
		}
		if err == nil {
			if keys == nil {
				keys = existing.APIKeys // 未给密钥：沿用
			}
			err = a.st.UpdateChannel(&store.Channel{
				ID: existing.ID, Name: ec.Name, Type: ec.Type, BaseURL: ec.BaseURL,
				APIKeys: keys, Models: ec.Models, DisabledModels: ec.DisabledModels,
				ModelMap: ec.ModelMap, Weight: ec.Weight, Priority: ec.Priority,
				Enabled: ec.Enabled, Remark: ec.Remark, CreatedAt: existing.CreatedAt,
			})
		} else {
			if keys == nil {
				keys = []store.ChannelKey{} // 新渠道：显式空（无密钥需手动补）
			}
			_, err = a.st.CreateChannel(&store.Channel{
				Name: ec.Name, Type: ec.Type, BaseURL: ec.BaseURL,
				APIKeys: keys, Models: ec.Models, DisabledModels: ec.DisabledModels,
				ModelMap: ec.ModelMap, Weight: ec.Weight, Priority: ec.Priority,
				Enabled: ec.Enabled, Remark: ec.Remark,
			})
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("渠道 %q: %v", ec.Name, err))
			continue
		}
		imported++
	}
	prices := 0
	for _, p := range in.Prices {
		if err := a.st.UpsertModelPrice(&store.ModelPrice{
			Model: p.Model, InputPrice: p.InputPrice, OutputPrice: p.OutputPrice,
			CacheReadPrice: p.CacheReadPrice, Currency: p.Currency, OffpeakRatio: p.OffpeakRatio,
		}); err != nil {
			errs = append(errs, fmt.Sprintf("价格 %q: %v", p.Model, err))
			continue
		}
		prices++
	}
	a.invalidateModels()
	a.invalidatePrices()
	resp := map[string]any{"channels_upserted": imported, "prices_upserted": prices}
	if len(errs) > 0 {
		resp["errors"] = errs
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- 渠道模型自动发现 ----

// discoverChannelModels 拉取上游模型列表：GET /api/admin/channels/{id}/discover → {models}。
// 供管理页"从上游拉取"按钮回填勾选列表。
func (a *admin) discoverChannelModels(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c, err := a.st.GetChannel(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}
	key := c.FirstKey()
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "channel has no enabled api key"})
		return
	}
	ctx := r.Context()
	ids, err := fetchFromChannel(ctx, upstreamClientForTest, c, key)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	sort.Strings(ids)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "models": ids})
}
