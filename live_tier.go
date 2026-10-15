package codextps

import (
	"strings"
	"time"
)

type liveTierSetting struct {
	At   time.Time
	Tier string
}

func (s *liveSession) applyServiceTier(tier string, at time.Time) {
	s.serviceTier = strings.ToLower(strings.TrimSpace(tier))
	s.serviceTiers = append(s.serviceTiers, liveTierSetting{at, s.serviceTier})
}

func (s *liveSession) serviceTierAt(at time.Time) (string, bool) {
	if at.IsZero() {
		return "", false
	}
	var found *liveTierSetting
	for i := range s.serviceTiers {
		setting := &s.serviceTiers[i]
		if !setting.At.After(at) && (found == nil || !setting.At.Before(found.At)) {
			found = setting
		}
	}
	if found == nil {
		return "", false
	}
	return found.Tier, true
}

// Codex subagent service tiers follow the root session preference. Resolve the
// recorded parent chain; never substitute global config or an unrelated thread.
func liveTierRoot(s *liveSession, sessions map[string]*liveFile) *liveFile {
	seen := map[string]bool{}
	for {
		if seen[s.ID] {
			return nil
		}
		seen[s.ID] = true
		f := sessions[s.ID]
		if f == nil {
			return nil
		}
		if s.ParentID == "" {
			return f
		}
		parent := sessions[s.ParentID]
		if parent == nil {
			return nil
		}
		s = parent.session
	}
}

func inheritedServiceTier(s *liveSession, sessions map[string]*liveFile, at time.Time, current bool) (string, string) {
	if s.ParentID == "" {
		return "", ""
	}
	root := liveTierRoot(s, sessions)
	if root == nil {
		return "", ""
	}
	if current {
		if root.offset < root.size || len(root.session.serviceTiers) == 0 {
			return "", ""
		}
		return root.session.serviceTier, "inherited"
	}
	tier, known := root.session.serviceTierAt(at)
	if !known {
		return "", ""
	}
	return tier, "inherited"
}

func fillResponseServiceTier(s *liveSession, response *liveResponse, sessions map[string]*liveFile) {
	if response.ServiceTierSource != "" {
		return
	}
	at := response.StartedAt
	if turn := s.turnsByID[response.TurnID]; turn != nil {
		at = turn.StartedAt
	}
	response.ServiceTier, response.ServiceTierSource = inheritedServiceTier(s, sessions, at, false)
}

func tierSessionView(f *liveFile, now time.Time, window int, sessions map[string]*liveFile, policies ...liveQuotaRules) liveSessionView {
	v := sessionView(f, now, window)
	rules := creditRules(policies)
	for _, turn := range f.session.Turns[windowStart(f.session, window):] {
		v.Credits.add(turnCredits(f.session, turn, sessions, rules))
	}
	if !v.Indexing && v.ServiceTierSource == "" {
		v.ServiceTier, v.ServiceTierSource = inheritedServiceTier(f.session, sessions, time.Time{}, true)
	}
	if v.LatestResponse != nil {
		copy := *v.LatestResponse
		fillResponseServiceTier(f.session, &copy, sessions)
		priceResponse(&copy, rules)
		v.LatestResponse = &copy
	}
	return v
}
