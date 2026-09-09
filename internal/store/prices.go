package store

import (
	"errors"
	"math"
	"strings"
	"time"
)

// ModelPrice 是单个模型的价格，input/output 单位均为「币种 / 百万 token」。
// input_price 为未命中缓存的输入价；cache_read_price 为缓存命中价，
// 0 表示未单独标价、自动按输入价 1/10 计（OpenAI 口径）。
// offpeak_ratio 为空闲时段折扣（DeepSeek 口径：北京时间周一至周五 9-12/14-18
// 之外按此折扣计费），1 表示不分时段。
// 币种跟随各渠道官方定价页（USD/CNY），不做汇率换算，只作标注与展示。
type ModelPrice struct {
	Model          string  `json:"model"`
	InputPrice     float64 `json:"input_price"`
	OutputPrice    float64 `json:"output_price"`
	CacheReadPrice float64 `json:"cache_read_price"`
	Currency       string  `json:"currency,omitempty"`
	OffpeakRatio   float64 `json:"offpeak_ratio"`
	UpdatedAt      string  `json:"updated_at"`
}

// 规范化：币种空值回退 USD 并转大写；空闲折扣 0 视为 1（不分时段）。
func (p *ModelPrice) normalize() {
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	if p.Currency == "" {
		p.Currency = "USD"
	}
	if p.OffpeakRatio <= 0 || p.OffpeakRatio > 1 {
		p.OffpeakRatio = 1
	}
}

func (s *Store) UpsertModelPrice(p *ModelPrice) error {
	p.Model = strings.TrimSpace(p.Model)
	p.normalize()
	if p.Model == "" {
		return errors.New("model is required")
	}
	_, err := s.DB.Exec(
		`INSERT INTO model_prices(model, input_price, output_price, cache_read_price, currency, offpeak_ratio, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, datetime('now'))
		 ON CONFLICT(model) DO UPDATE SET
		   input_price      = excluded.input_price,
		   output_price     = excluded.output_price,
		   cache_read_price = excluded.cache_read_price,
		   currency         = excluded.currency,
		   offpeak_ratio    = excluded.offpeak_ratio,
		   updated_at       = datetime('now')`,
		p.Model, p.InputPrice, p.OutputPrice, p.CacheReadPrice, p.Currency, p.OffpeakRatio)
	return err
}

func (s *Store) ListModelPrices() ([]ModelPrice, error) {
	rows, err := s.DB.Query(
		`SELECT model, input_price, output_price, cache_read_price, currency, offpeak_ratio, updated_at FROM model_prices ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelPrice{}
	for rows.Next() {
		var p ModelPrice
		if err := rows.Scan(&p.Model, &p.InputPrice, &p.OutputPrice, &p.CacheReadPrice, &p.Currency, &p.OffpeakRatio, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DeleteModelPrice(model string) error {
	res, err := s.DB.Exec(`DELETE FROM model_prices WHERE model = ?`, model)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CostOf 按价格行自身的币种与单价计算单笔请求成本（币种间不做换算）；
// 无价格信息时成本记 0。
// promptTokens 为总输入 token（含缓存命中与写入）；缓存读优先用 cache_read_price，
// 未单独标价（=0）时按输入价 1/10；缓存写按输入价 1.25 倍计（OpenAI / Anthropic 口径）。
// 配置了空闲折扣（offpeak_ratio<1）的模型按北京时间高峰时段表判定：
// 周一至周五 9:00-12:00、14:00-18:00 全价，其余时段按折扣计。
// 结果四舍五入到小数点后 6 位，避免浮点尾巴进日志和聚合。
func CostOf(p *ModelPrice, at time.Time, promptTokens, cacheRead, cacheWrite, completionTokens int64) float64 {
	if p == nil {
		return 0
	}
	ratio := 1.0
	if p.OffpeakRatio > 0 && p.OffpeakRatio < 1 && !inPeakBeijing(at) {
		ratio = p.OffpeakRatio
	}
	input := p.InputPrice * ratio
	output := p.OutputPrice * ratio
	readPrice := p.CacheReadPrice
	if readPrice <= 0 {
		readPrice = input * 0.1
	} else {
		readPrice *= ratio
	}
	uncached := promptTokens - cacheRead - cacheWrite
	if uncached < 0 {
		uncached = 0
	}
	cost := float64(uncached)/1e6*input +
		float64(cacheRead)/1e6*readPrice +
		float64(cacheWrite)/1e6*input*1.25 +
		float64(completionTokens)/1e6*output
	return math.Round(cost*1e6) / 1e6
}

// MatchPrice 在价格表里找模型单价：先精确匹配，再退回最长的段边界前缀，
// 如 "gpt-4o" 覆盖 "gpt-4o-2024-08-06"；"gpt-4" 不会匹配 "gpt-4o"。
func MatchPrice(prices []ModelPrice, model string) *ModelPrice {
	if model == "" {
		return nil
	}
	var best *ModelPrice
	for i := range prices {
		p := &prices[i]
		if p.Model == model {
			return p
		}
		if strings.HasPrefix(model, p.Model+"-") ||
			strings.HasPrefix(model, p.Model+".") ||
			strings.HasPrefix(model, p.Model+"/") {
			if best == nil || len(p.Model) > len(best.Model) {
				best = p
			}
		}
	}
	return best
}

// inPeakBeijing 报告 t 对应的北京时间是否处于高峰时段
// （DeepSeek 口径：周一至周五 09:00-12:00、14:00-18:00；周末全天与其余时间为空闲）。
func inPeakBeijing(t time.Time) bool {
	b := t.In(time.FixedZone("CST", 8*3600))
	if wd := b.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	hm := b.Hour()*60 + b.Minute()
	return (hm >= 9*60 && hm < 12*60) || (hm >= 14*60 && hm < 18*60)
}
