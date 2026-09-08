package store

import (
	"errors"
	"math"
	"strings"
)

// ModelPrice 是单个模型的价格，input/output 单位均为「币种 / 百万 token」。
// 币种跟随各渠道官方定价页（USD/CNY），不做汇率换算，只作标注与展示。
type ModelPrice struct {
	Model       string  `json:"model"`
	InputPrice  float64 `json:"input_price"`
	OutputPrice float64 `json:"output_price"`
	Currency    string  `json:"currency,omitempty"`
	UpdatedAt   string  `json:"updated_at"`
}

// 规范化币种：空值回退 USD，其余转大写；未知币种由调用方校验。
func (p *ModelPrice) normalizeCurrency() {
	p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
	if p.Currency == "" {
		p.Currency = "USD"
	}
}

func (s *Store) UpsertModelPrice(p *ModelPrice) error {
	p.Model = strings.TrimSpace(p.Model)
	p.normalizeCurrency()
	if p.Model == "" {
		return errors.New("model is required")
	}
	_, err := s.DB.Exec(
		`INSERT INTO model_prices(model, input_price, output_price, currency, updated_at)
		 VALUES(?, ?, ?, ?, datetime('now'))
		 ON CONFLICT(model) DO UPDATE SET
		   input_price  = excluded.input_price,
		   output_price = excluded.output_price,
		   currency     = excluded.currency,
		   updated_at   = datetime('now')`,
		p.Model, p.InputPrice, p.OutputPrice, p.Currency)
	return err
}

func (s *Store) ListModelPrices() ([]ModelPrice, error) {
	rows, err := s.DB.Query(
		`SELECT model, input_price, output_price, currency, updated_at FROM model_prices ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelPrice{}
	for rows.Next() {
		var p ModelPrice
		if err := rows.Scan(&p.Model, &p.InputPrice, &p.OutputPrice, &p.Currency, &p.UpdatedAt); err != nil {
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
// promptTokens 为总输入 token（含缓存命中与写入）；缓存读按输入价 1/10、
// 缓存写按输入价 1.25 倍计（OpenAI / Anthropic 官方口径一致）。
// 结果四舍五入到小数点后 6 位，避免浮点尾巴进日志和聚合。
func CostOf(p *ModelPrice, promptTokens, cacheRead, cacheWrite, completionTokens int64) float64 {
	if p == nil {
		return 0
	}
	uncached := promptTokens - cacheRead - cacheWrite
	if uncached < 0 {
		uncached = 0
	}
	cost := float64(uncached)/1e6*p.InputPrice +
		float64(cacheRead)/1e6*p.InputPrice*0.1 +
		float64(cacheWrite)/1e6*p.InputPrice*1.25 +
		float64(completionTokens)/1e6*p.OutputPrice
	return math.Round(cost*1e6) / 1e6
}
