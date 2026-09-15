package api

// 上游余额探测：按渠道配置的 balance_api 协议逐把密钥查询剩余额度。
// - deepseek:   GET  {root}/user/balance（base_url 去掉版本前缀），余额在 balance_infos[0]
// - openrouter: GET  {base}/credits，remaining = total_credits - total_usage（USD）
// 结果缓存 30 分钟供告警判断复用；余额低于渠道阈值时推送告警（每把密钥每天一次）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"litegate/internal/store"
)

// balanceAPIs 是支持的余额查询协议（渠道编辑下拉用）。
var balanceAPIs = map[string]bool{"": true, "deepseek": true, "openrouter": true}

type keyBalance struct {
	KeyID     int64   `json:"key_id"`
	Masked    string  `json:"masked"`
	Remaining float64 `json:"remaining"`
	Currency  string  `json:"currency"`
	Error     string  `json:"error,omitempty"`
}

type balanceReport struct {
	At    time.Time    `json:"at"`
	Keys  []keyBalance `json:"keys"`
	Error string       `json:"error,omitempty"`
}

// fetchBalance 查询单把密钥的余额（剩余额度 + 币种）。
func fetchBalance(ctx context.Context, client *http.Client, c *store.Channel, key string) (float64, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var url string
	switch c.BalanceAPI {
	case "deepseek":
		// base_url 形如 https://api.deepseek.com/v1，余额端点在版本前缀之外的根路径
		root := strings.TrimSuffix(strings.TrimRight(c.BaseURL, "/"), "/v1")
		url = root + "/user/balance"
	case "openrouter":
		url = strings.TrimRight(c.BaseURL, "/") + "/credits"
	default:
		return 0, "", fmt.Errorf("balance api not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return 0, "", fmt.Errorf("upstream status %d", resp.StatusCode)
	}
	switch c.BalanceAPI {
	case "deepseek":
		var v struct {
			BalanceInfos []struct {
				Currency     string `json:"currency"`
				TotalBalance string `json:"total_balance"`
			} `json:"balance_infos"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
			return 0, "", err
		}
		if len(v.BalanceInfos) == 0 {
			return 0, "", fmt.Errorf("no balance info")
		}
		amount, err := strconv.ParseFloat(v.BalanceInfos[0].TotalBalance, 64)
		if err != nil {
			return 0, "", err
		}
		return amount, v.BalanceInfos[0].Currency, nil
	default: // openrouter
		var v struct {
			Data struct {
				TotalCredits float64 `json:"total_credits"`
				TotalUsage   float64 `json:"total_usage"`
			} `json:"data"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
			return 0, "", err
		}
		return v.Data.TotalCredits - v.Data.TotalUsage, "USD", nil
	}
}

// probeChannelBalances 逐把启用密钥查询余额（并发封顶 8），结果实时返回给
// 管理端并就地做阈值告警判断；不做缓存（管理台查询是低频手动操作）。
func (p *proxy) probeChannelBalances(ctx context.Context, c *store.Channel) balanceReport {
	var enabled []store.ChannelKey
	for _, k := range c.APIKeys {
		if k.Enabled {
			enabled = append(enabled, k)
		}
	}
	report := balanceReport{At: time.Now(), Keys: make([]keyBalance, 0, len(enabled))}
	if len(enabled) == 0 {
		report.Error = "no enabled keys"
		return report
	}
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	var mu sync.Mutex // 保护 report.Keys 追加顺序无关，只防并发写
	for _, k := range enabled {
		wg.Add(1)
		go func(k store.ChannelKey) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			kb := keyBalance{KeyID: k.ID, Masked: k.Masked}
			amount, cur, err := fetchBalance(ctx, p.client, c, k.Key)
			if err != nil {
				kb.Error = err.Error()
			} else {
				kb.Remaining, kb.Currency = amount, cur
			}
			mu.Lock()
			report.Keys = append(report.Keys, kb)
			mu.Unlock()
		}(k)
	}
	wg.Wait()

	// 阈值告警：由调用方（admin handler）在拿到渠道配置后触发，这里不做
	return report
}

// maybeAlertBalance 按渠道阈值对探测结果做告警判断。
func (p *proxy) maybeAlertBalance(c *store.Channel, r balanceReport) {
	if c.BalanceAlertBelow <= 0 || p.alerts == nil {
		return
	}
	for _, kb := range r.Keys {
		if kb.Error == "" && kb.Remaining < c.BalanceAlertBelow {
			p.alerts.fireBalance(c.Name, kb.Masked, kb.Remaining, kb.Currency, c.BalanceAlertBelow)
		}
	}
}

// ---- 管理端点 ----

func (a *admin) channelBalances(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c, err := a.st.GetChannel(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel not found"})
		return
	}
	if c.BalanceAPI == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "balance api not configured for this channel"})
		return
	}
	report := a.proxyForBalance().probeChannelBalances(r.Context(), c)
	a.proxyForBalance().maybeAlertBalance(c, report)
	writeJSON(w, http.StatusOK, report)
}

// proxyForBalance 余额探测复用数据面的 client 与缓存（进程内单实例）。
func (a *admin) proxyForBalance() *proxy { return serverProxy }

// ---- 告警配置端点 ----

func (a *admin) getAlerts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.alerts.Config())
}

func (a *admin) putAlerts(w http.ResponseWriter, r *http.Request) {
	var cfg AlertsConfig
	if readJSON(w, r, &cfg) != nil {
		return
	}
	if err := a.alerts.Save(cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.audit(r, "alerts.update", "enabled="+strconv.FormatBool(cfg.Enabled))
	writeJSON(w, http.StatusOK, a.alerts.Config())
}

func (a *admin) testAlert(w http.ResponseWriter, _ *http.Request) {
	if err := a.alerts.TestAlert(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}
