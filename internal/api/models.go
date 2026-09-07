package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"litegate/internal/store"
)

type modelItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

type modelsCache struct {
	mu   sync.Mutex
	at   time.Time
	data []modelItem
}

func (p *proxy) serveModels(w http.ResponseWriter, r *http.Request) {
	ak, err := p.authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid api key"})
		return
	}
	data := p.aggregateModels(r.Context())
	// 密钥配置了模型白名单时,只返回交集
	if len(ak.AllowedModels) > 0 {
		filtered := data[:0:0]
		for _, m := range data {
			if containsModelStr(ak.AllowedModels, m.ID) {
				filtered = append(filtered, m)
			}
		}
		data = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// invalidateModelsCache 使聚合缓存失效（管理面增删渠道、启停模型时调用）。
func (p *proxy) invalidateModelsCache() {
	p.cache.mu.Lock()
	p.cache.data = nil
	p.cache.at = time.Time{}
	p.cache.mu.Unlock()
}

// invalidatePriceCache 使价格缓存失效（管理面改价时调用，否则成本核算滞后 60 秒）。
func (p *proxy) invalidatePriceCache() {
	p.pc.mu.Lock()
	p.pc.prices = nil
	p.pc.expires = time.Time{}
	p.pc.mu.Unlock()
}

func containsModelStr(list []string, s string) bool {
	for _, m := range list {
		if m == s {
			return true
		}
	}
	return false
}

// aggregateModels 返回全部"启用中"的模型：显式列出 models 的渠道直接用配置列表
// （禁用模型已不在其中），通配渠道拉上游列表并剔除禁用项；结果缓存 60 秒，
// 管理面变更渠道/启停模型时会主动失效。
func (p *proxy) aggregateModels(ctx context.Context) []modelItem {
	p.cache.mu.Lock()
	if p.cache.data != nil && time.Since(p.cache.at) < time.Minute {
		data := p.cache.data
		p.cache.mu.Unlock()
		return data
	}
	p.cache.mu.Unlock()

	chans, err := p.st.ListChannels("openai")
	if err != nil {
		return p.cachedModels()
	}
	chans = enabledOnly(chans)
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		out  []modelItem
		seen = map[string]bool{}
	)
	add := func(id, owner string) {
		mu.Lock()
		defer mu.Unlock()
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, modelItem{ID: id, Object: "model", OwnedBy: owner})
		}
	}
	for _, c := range chans {
		if len(c.Models) > 0 {
			for _, id := range c.Models {
				add(id, c.Name)
			}
			continue
		}
		wg.Add(1)
		go func(c store.Channel) {
			defer wg.Done()
			for _, id := range fetchChannelModels(ctx, p.client, &c) {
				if containsModelStr(c.DisabledModels, id) {
					continue
				}
				add(id, c.Name)
			}
		}(c)
	}
	wg.Wait()

	p.cache.mu.Lock()
	if len(out) > 0 {
		p.cache.at = time.Now()
		p.cache.data = out
	}
	data := p.cache.data
	p.cache.mu.Unlock()
	if data == nil {
		// 防止 {"data":null} 打崩客户端解析
		data = []modelItem{}
	}
	return data
}

func (p *proxy) cachedModels() []modelItem {
	p.cache.mu.Lock()
	defer p.cache.mu.Unlock()
	if p.cache.data == nil {
		return []modelItem{}
	}
	return p.cache.data
}

func fetchUpstreamModels(ctx context.Context, client *http.Client, c *store.Channel) (int, error) {
	ids, err := fetchFromChannel(ctx, client, c)
	return len(ids), err
}

func fetchChannelModels(ctx context.Context, client *http.Client, c *store.Channel) []string {
	ids, err := fetchFromChannel(ctx, client, c)
	if err != nil {
		return nil
	}
	return ids
}

// fetchFromChannel 请求 GET {base_url}/models：openai 渠道用 Bearer，anthropic 渠道用 x-api-key。
func fetchFromChannel(ctx context.Context, client *http.Client, c *store.Channel) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/models", nil)
	if err != nil {
		return nil, err
	}
	switch c.Type {
	case "anthropic":
		req.Header.Set("X-Api-Key", c.APIKey)
		req.Header.Set("Anthropic-Version", "2023-06-01")
	default:
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	var mr struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&mr); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(mr.Data))
	for _, m := range mr.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}
