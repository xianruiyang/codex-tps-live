package codextps

import (
	"net/http"
	"time"
)

type liveQuotaForecast struct {
	Status           string     `json:"status"`
	Message          string     `json:"message"`
	RangeFrom        time.Time  `json:"range_from"`
	RangeTo          time.Time  `json:"range_to"`
	SampleFrom       *time.Time `json:"sample_from,omitempty"`
	SampleTo         *time.Time `json:"sample_to,omitempty"`
	AsOf             time.Time  `json:"as_of"`
	RemainingPercent float64    `json:"remaining_percent"`
	DeltaPercent     float64    `json:"delta_percent"`
	Points           int        `json:"points"`
	RatePerHour      *float64   `json:"percent_per_hour"`
	SecondsRemaining *float64   `json:"seconds_remaining"`
	ExhaustsAt       *time.Time `json:"exhausts_at"`
	ResetsAt         int64      `json:"resets_at"`
	ResetsFirst      bool       `json:"resets_first"`
}

// Forecast from observed quota percentages, independently of credit calibration
// and token pricing. The input is the original, segmented sequence, not chart dots.
func forecastQuota(group *liveQuotaSeries, r *http.Request, from, to time.Time, pending int) liveQuotaForecast {
	f := liveQuotaForecast{Status: "insufficient", RangeFrom: from, RangeTo: to, Message: "参考区间内至少需要两条有效额度快照，请扩大区间或等待上报。"}
	query := r.URL.Query()
	if query.Get("forecast_from") != "" || query.Get("forecast_to") != "" {
		start, startErr := time.Parse(time.RFC3339Nano, query.Get("forecast_from"))
		end, endErr := time.Parse(time.RFC3339Nano, query.Get("forecast_to"))
		if startErr != nil || endErr != nil || !start.Before(end) || start.Before(from) || end.After(to) {
			f.Status, f.Message = "invalid_range", "请选择图表范围内的有效起止时间。"
			return f
		}
		f.RangeFrom, f.RangeTo = start, end
	}
	var latest *liveQuotaPoint
	for i := len(group.Points) - 1; i >= 0; i-- {
		if !group.Points[i].Stale {
			latest = &group.Points[i]
			break
		}
	}
	if latest == nil {
		return f
	}
	f.AsOf, f.RemainingPercent, f.ResetsAt = latest.At, latest.RemainingPercent, latest.ResetsAt
	if latest.Expired {
		f.Status, f.Message = "expired", "最近快照已超过重置时间，等待新的额度上报后再预测。"
		return f
	}
	if latest.RemainingPercent <= 0 {
		f.Status, f.Message, f.ExhaustsAt = "exhausted", "最近有效快照中的额度已耗尽。", &latest.At
		return f
	}
	var first, last *liveQuotaPoint
	for i := range group.Points {
		p := &group.Points[i]
		if p.Stale || p.At.Before(f.RangeFrom) || p.At.After(f.RangeTo) {
			continue
		}
		if p.Segment != latest.Segment {
			f.Status, f.Message = "discontinuous", "参考区间跨越重置、回退或上报空窗，请在图上选取当前连续段内的区间。"
			return f
		}
		if first == nil {
			first = p
		}
		last = p
		f.Points++
	}
	if first == nil || last == nil || !first.At.Before(last.At) {
		return f
	}
	f.SampleFrom, f.SampleTo = &first.At, &last.At
	f.DeltaPercent = last.UsedPercent - first.UsedPercent
	if f.DeltaPercent <= 0 {
		f.Status, f.Message = "no_consumption", "参考区间内未观察到额度消耗，暂无法给出有限的耗尽时间。"
		return f
	}
	rate := f.DeltaPercent / last.At.Sub(first.At).Hours()
	seconds := latest.RemainingPercent / rate * 3600
	f.RatePerHour, f.SecondsRemaining = quotaPointer(rate), quotaPointer(seconds)
	if seconds > float64(time.Duration(1<<63-1)/time.Second) {
		f.Status, f.Message = "too_slow", "观测到的消耗速率过低，耗尽日期超出可表示范围。"
		return f
	}
	at := latest.At.Add(time.Duration(seconds * float64(time.Second)))
	f.ExhaustsAt = &at
	f.ResetsFirst = latest.ResetsAt > 0 && !at.Before(time.Unix(latest.ResetsAt, 0))
	f.Status, f.Message = "estimated", "按参考区间的平均消耗速率线性外推；未来使用强度变化会改变结果。"
	if f.DeltaPercent < 3 || pending > 0 {
		f.Status, f.Message = "provisional", "暂定预测：百分点变化较少或历史仍在补读，结果可能明显波动。"
	}
	return f
}
