package domino

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"sync"
)

// memoryStore keeps rules in memory. It copies rules in and out, like a
// database would, so callers can't change what's stored.
type memoryStore struct {
	mu    sync.Mutex
	next  int
	rules []*ownedRule
}

type ownedRule struct {
	owner string
	rule  *Rule
}

var _ Store = (*memoryStore)(nil)

func (m *memoryStore) Save(ctx context.Context, owner string, rule *Rule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := -1
	if rule.ID != "" {
		if i = m.find(owner, rule.ID); i < 0 {
			return ErrNotFound
		}
	}
	stored, err := clone(rule)
	if err != nil {
		return err
	}
	if i >= 0 {
		m.rules[i].rule = stored
		return nil
	}
	m.next++
	rule.ID = strconv.Itoa(m.next)
	stored.ID = rule.ID
	m.rules = append(m.rules, &ownedRule{owner, stored})
	return nil
}

func (m *memoryStore) List(ctx context.Context, owner string, triggers ...string) ([]*Rule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var rules []*Rule
	for _, owned := range m.rules {
		if owned.owner != owner {
			continue
		}
		if len(triggers) > 0 && !slices.Contains(triggers, owned.rule.Trigger) {
			continue
		}
		rule, err := clone(owned.rule)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

func (m *memoryStore) Delete(ctx context.Context, owner, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := m.find(owner, id)
	if i < 0 {
		return ErrNotFound
	}
	m.rules = slices.Delete(m.rules, i, i+1)
	return nil
}

func (m *memoryStore) find(owner, id string) int {
	return slices.IndexFunc(m.rules, func(owned *ownedRule) bool {
		return owned.owner == owner && owned.rule.ID == id
	})
}

// clone deep-copies a rule through JSON, as a database round trip would
func clone(rule *Rule) (*Rule, error) {
	b, err := json.Marshal(rule)
	if err != nil {
		return nil, err
	}
	out := new(Rule)
	if err := json.Unmarshal(b, out); err != nil {
		return nil, err
	}
	return out, nil
}
