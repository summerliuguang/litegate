package store

import (
	"errors"
	"math"
	"strings"
)

// ModelPrice 是单个模型的价格，input/output 单位均为「美元 / 百万 token」，
// 与 OpenAI 官方定价页及主流中转站的报价口径一致。
type ModelPrice struct {
	Model       string  `json:"model"`
	InputPrice  float64 `json:"input_price"`
	OutputPrice float64 `json:"output_price"`
	UpdatedAt   string  `json:"updated_at"`
}

func (s *Store) UpsertModelPrice(p *ModelPrice) error {
	p.Model = strings.TrimSpace(p.Model)
	if p.Model == "" {
		return errors.New("model is required")
	}
	_, err := s.DB.Exec(
		`INSERT INTO model_prices(model, input_price, output_price, updated_at)
		 VALUES(?, ?, ?, datetime('now'))
		 ON CONFLICT(model) DO UPDATE SET
		   input_price  = excluded.input_price,
		   output_price = excluded.output_price,
		   updated_at   = datetime('now')`,
		p.Model, p.InputPrice, p.OutputPrice)
	return err
}

func (s *Store) ListModelPrices() ([]ModelPrice, error) {
	rows, err := s.DB.Query(
		`SELECT model, input_price, output_price, updated_at FROM model_prices ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelPrice{}
	for rows.Next() {
		var p ModelPrice
		if err := rows.Scan(&p.Model, &p.InputPrice, &p.OutputPrice, &p.UpdatedAt); err != nil {
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

// CostOf 按「美元 / 百万 token」计算单笔请求成本；无价格信息时成本记 0。
// 结果四舍五入到小数点后 6 位，避免浮点尾巴进日志和聚合。
func CostOf(p *ModelPrice, promptTokens, completionTokens int64) float64 {
	if p == nil {
		return 0
	}
	cost := float64(promptTokens)/1e6*p.InputPrice + float64(completionTokens)/1e6*p.OutputPrice
	return math.Round(cost*1e6) / 1e6
}
