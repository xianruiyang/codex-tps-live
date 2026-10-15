package codextps

// Credits are an estimate at the selected Codex credit rates. Unknown model or
// speed data stays unpriced, rather than silently contributing zero cost.
type liveCreditUsage struct {
	Value       *float64 `json:"value"`
	Priced      int      `json:"priced_responses"`
	Unpriced    int      `json:"unpriced_responses"`
	LongContext int      `json:"long_context_responses"`
}

type liveCreditPolicy struct {
	Basis   string  `json:"basis"`
	Context string  `json:"context_policy"`
	AsOf    string  `json:"as_of"`
	Fast    float64 `json:"fast_multiplier"`
	Ultra   float64 `json:"ultra_multiplier"`
}

func creditRules(policies []liveQuotaRules) liveQuotaRules {
	if len(policies) > 0 {
		return policies[0]
	}
	return quotaRules(nil)
}

func creditPolicy(rules liveQuotaRules) liveCreditPolicy {
	return liveCreditPolicy{Basis: rules.Basis, Context: rules.Context, AsOf: rules.AsOf, Fast: rules.Fast, Ultra: rules.Ultra}
}

func priceResponse(response *liveResponse, rules liveQuotaRules) liveCreditUsage {
	cost, priced, long := quotaCost(liveQuotaUsage{Model: response.Model, ServiceTier: response.ServiceTier, Tokens: response.Tokens}, rules)
	usage := liveCreditUsage{}
	if long {
		usage.LongContext = 1
	}
	if priced {
		usage.Value, usage.Priced = quotaPointer(cost), 1
	} else {
		usage.Unpriced = 1
	}
	response.Credits = &usage
	return usage
}

func (usage *liveCreditUsage) add(other liveCreditUsage) {
	if other.Value != nil {
		if usage.Value == nil {
			usage.Value = quotaPointer(0)
		}
		*usage.Value += *other.Value
	}
	usage.Priced += other.Priced
	usage.Unpriced += other.Unpriced
	usage.LongContext += other.LongContext
}

func turnCredits(s *liveSession, turn *liveTurn, sessions map[string]*liveFile, rules liveQuotaRules) liveCreditUsage {
	usage := liveCreditUsage{}
	for _, response := range turn.Responses {
		// Resolve the response's historical tier, including subagent inheritance,
		// before pricing. Do not use a turn's latest model or speed for all rows.
		fillResponseServiceTier(s, &response, sessions)
		usage.add(priceResponse(&response, rules))
	}
	return usage
}
