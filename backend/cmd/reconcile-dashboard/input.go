package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

const maxInputBytes = 1 << 20

var beijing = time.FixedZone("Asia/Shanghai", 8*60*60)

type revenueRow struct {
	Date    string  `json:"date"`
	Revenue float64 `json:"revenue"`
}

type repairInput struct {
	UserID         string       `json:"userId"`
	AdminAccountID string       `json:"adminAccountId"`
	Rows           []revenueRow `json:"rows"`
}

func decodeInput(reader io.Reader, now time.Time) (repairInput, error) {
	var result repairInput
	raw, err := io.ReadAll(io.LimitReader(reader, maxInputBytes+1))
	if err != nil || len(raw) > maxInputBytes {
		return result, errors.New("输入读取失败或超过1MiB")
	}
	// 标准结构解码会接受重复对象键，先拒收歧义，避免审阅值与执行值不同。
	if err := rejectDuplicateKeys(json.NewDecoder(bytes.NewReader(raw)), 0); err != nil {
		return result, errors.New("JSON存在重复字段或语法错误")
	}
	var wire struct {
		UserID         string `json:"userId"`
		AdminAccountID string `json:"adminAccountId"`
		Rows           []struct {
			Date    string   `json:"date"`
			Revenue *float64 `json:"revenue"`
		} `json:"rows"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF {
		return result, errors.New("JSON字段、金额或尾随内容无效")
	}
	if wire.UserID == "" || wire.AdminAccountID == "" || strings.TrimSpace(wire.UserID) != wire.UserID || strings.TrimSpace(wire.AdminAccountID) != wire.AdminAccountID || len(wire.UserID) > 256 || len(wire.AdminAccountID) > 256 {
		return result, errors.New("必须明确指定有效userId与adminAccountId")
	}
	if len(wire.Rows) < 1 || len(wire.Rows) > 366 {
		return result, errors.New("日期行数必须为1至366")
	}
	result.UserID, result.AdminAccountID = wire.UserID, wire.AdminAccountID
	today := now.In(beijing).Format(time.DateOnly)
	var total float64
	for _, row := range wire.Rows {
		_, err := time.Parse(time.DateOnly, row.Date)
		if err != nil || row.Date >= today || row.Revenue == nil || !finite(*row.Revenue) || *row.Revenue < 0 {
			return repairInput{}, errors.New("仅接受过去自然日和有限非负营收")
		}
		total += *row.Revenue
		result.Rows = append(result.Rows, revenueRow{Date: row.Date, Revenue: *row.Revenue})
	}
	if !finite(total) {
		return repairInput{}, errors.New("营收合计溢出")
	}
	sort.Slice(result.Rows, func(i, j int) bool { return result.Rows[i].Date < result.Rows[j].Date })
	for i := 1; i < len(result.Rows); i++ {
		previous, _ := time.Parse(time.DateOnly, result.Rows[i-1].Date)
		if previous.AddDate(0, 0, 1).Format(time.DateOnly) != result.Rows[i].Date {
			return repairInput{}, errors.New("日期必须连续且不得重复")
		}
	}
	return result, nil
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func rejectDuplicateKeys(decoder *json.Decoder, depth int) error {
	if depth > 8 {
		return errors.New("输入嵌套过深")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] || (name != "userId" && name != "adminAccountId" && name != "rows" && name != "date" && name != "revenue") {
				return errors.New("重复字段")
			}
			seen[name] = true
			if err := rejectDuplicateKeys(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := rejectDuplicateKeys(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("无效JSON结构")
	}
	_, err = decoder.Token()
	return err
}
