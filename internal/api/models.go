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
	if _, err := p.authenticate(r); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid api key"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": p.aggregateModels(r.Context())})
}

// aggregateModels 并发拉取所有 openai 渠道的模型列表并按 id 去重，结果缓存 60 秒。
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
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		out  []modelItem
		seen = map[string]bool{}
	)
	for _, c := range chans {
		wg.Add(1)
		go func(c store.Channel) {
			defer wg.Done()
			for _, id := range fetchChannelModels(ctx, p.client, &c) {
				mu.Lock()
				if !seen[id] {
					seen[id] = true
					out = append(out, modelItem{ID: id, Object: "model", OwnedBy: c.Name})
				}
				mu.Unlock()
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
	return data
}

func (p *proxy) cachedModels() []modelItem {
	p.cache.mu.Lock()
	defer p.cache.mu.Unlock()
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
