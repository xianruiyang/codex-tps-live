package codextps

import (
	"context"
	"sort"
	"time"
)

type liveQuotaCapacityHistory struct {
	LookbackSeconds float64                  `json:"lookback_seconds"`
	StepSeconds     float64                  `json:"step_seconds"`
	Points          []liveQuotaCapacityPoint `json:"points"`
}

type liveQuotaCapacityPoint struct {
	At           time.Time `json:"at"`
	FullCredits  *float64  `json:"full_credits"`
	Intervals    int       `json:"intervals"`
	DeltaPercent float64   `json:"delta_percent"`
}

func rollingQuotaCapacity(ctx context.Context, group *liveQuotaSeries, records []liveQuotaRecord, rules liveQuotaRules, from, to time.Time, lookback time.Duration) liveQuotaCapacityHistory {
	history := liveQuotaCapacityHistory{LookbackSeconds: lookback.Seconds(), Points: []liveQuotaCapacityPoint{}}
	if group == nil || from.After(to) {
		return history
	}
	const targetIntervals = 120
	step := max(time.Minute, ((to.Sub(from)/targetIntervals+time.Minute-1)/time.Minute)*time.Minute)
	history.StepSeconds = step.Seconds()

	scratch := make([]liveQuotaPoint, len(group.Points))
	intervalCosts := make(map[[2]time.Time]float64)
	for at := from; ; at = at.Add(step) {
		if ctx.Err() != nil {
			break
		}
		if at.After(to) {
			at = to
		}
		result := liveQuotaCapacityPoint{At: at}
		end := sort.Search(len(group.Points), func(i int) bool { return group.Points[i].At.After(at) })
		if end > 1 {
			// A later snapshot may confirm a rollback; classify only the
			// records available through this point's timestamp.
			points := scratch[:end]
			copy(points, group.Points[:end])
			classifyQuotaPoints(points)
			latest := points[end-1]
			var credits float64
			forEachQuotaChange(points, at.Add(-lookback), func(anchor, point liveQuotaPoint, excluded bool) {
				if excluded || !quotaCalibrationCompatible(point, latest) {
					return
				}
				key := [2]time.Time{anchor.At, point.At}
				cost, known := intervalCosts[key]
				if !known {
					cost, _, _, _, _, _ = quotaUsageBetween(records, group.Bucket, anchor.At, point.At, rules)
					intervalCosts[key] = cost
				}
				if cost > 0 {
					credits += cost
					result.DeltaPercent += point.UsedPercent - anchor.UsedPercent
					result.Intervals++
				}
			})
			if result.DeltaPercent > 0 {
				result.FullCredits = quotaPointer(credits * 100 / result.DeltaPercent)
			}
		}
		history.Points = append(history.Points, result)
		if at.Equal(to) {
			break
		}
	}
	return history
}
