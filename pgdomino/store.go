package pgdomino

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/matthewmueller/domino"
	"github.com/matthewmueller/domino/pgdomino/internal/pogo/dominoaction"
	"github.com/matthewmueller/domino/pgdomino/internal/pogo/dominocondition"
	"github.com/matthewmueller/domino/pgdomino/internal/pogo/dominorule"
)

// store keeps rules in domino_rules, with their conditions and actions in
// domino_conditions and domino_actions, ordered by position.
//
// The generated pogo queries don't take a context yet (they use
// context.TODO), so only transactions respect ctx.
type store struct {
	db *pgxpool.Pool
}

var _ domino.Store = (*store)(nil)

func (s *store) List(ctx context.Context, ownerID string, triggers ...string) ([]*domino.Rule, error) {
	filter := dominorule.NewFilter().OwnerID(ownerID)
	if len(triggers) > 0 {
		filter = filter.TriggerIn(triggers...)
	}
	rows, err := dominorule.FindMany(s.db, filter, dominorule.NewOrder().ID(dominorule.ASC))
	if err != nil {
		return nil, fmt.Errorf("pgdomino: listing rules: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(rows))
	rules := make([]*domino.Rule, len(rows))
	byID := make(map[int64]*domino.Rule, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
		rules[i] = &domino.Rule{ID: strconv.FormatInt(row.ID, 10), Name: row.Name, Trigger: row.Trigger}
		byID[row.ID] = rules[i]
	}
	conditions, err := dominocondition.FindMany(s.db,
		dominocondition.NewFilter().RuleIDIn(ids...),
		dominocondition.NewOrder().RuleID(dominocondition.ASC).Position(dominocondition.ASC),
	)
	if err != nil {
		return nil, fmt.Errorf("pgdomino: listing conditions: %w", err)
	}
	for _, row := range conditions {
		var value any
		if err := json.Unmarshal(row.Value, &value); err != nil {
			return nil, fmt.Errorf("pgdomino: decoding condition %d: %w", row.ID, err)
		}
		r := byID[row.RuleID]
		r.Conditions = append(r.Conditions, &domino.Condition{Field: row.Field, Op: row.Op, Value: value})
	}
	actions, err := dominoaction.FindMany(s.db,
		dominoaction.NewFilter().RuleIDIn(ids...),
		dominoaction.NewOrder().RuleID(dominoaction.ASC).Position(dominoaction.ASC),
	)
	if err != nil {
		return nil, fmt.Errorf("pgdomino: listing actions: %w", err)
	}
	for _, row := range actions {
		r := byID[row.RuleID]
		r.Actions = append(r.Actions, &domino.Action{Name: row.Name, Settings: row.Settings})
	}
	return rules, nil
}

func (s *store) Save(ctx context.Context, ownerID string, r *domino.Rule) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgdomino: unable to begin: %w", err)
	}
	defer tx.Rollback(ctx)
	id, err := saveRule(tx, ownerID, r)
	if err != nil {
		return err
	}
	// replace the rule's conditions and actions
	if _, err := dominocondition.DeleteMany(tx, dominocondition.NewFilter().RuleID(id)); err != nil {
		return fmt.Errorf("pgdomino: deleting conditions: %w", err)
	}
	if _, err := dominoaction.DeleteMany(tx, dominoaction.NewFilter().RuleID(id)); err != nil {
		return fmt.Errorf("pgdomino: deleting actions: %w", err)
	}
	for i, c := range r.Conditions {
		value, err := json.Marshal(c.Value)
		if err != nil {
			return fmt.Errorf("pgdomino: encoding condition %d: %w", i, err)
		}
		input := dominocondition.New().RuleID(id).Position(i).Field(c.Field).Op(c.Op).Value(value)
		if _, err := dominocondition.Insert(tx, input); err != nil {
			return fmt.Errorf("pgdomino: inserting condition %d: %w", i, err)
		}
	}
	for i, a := range r.Actions {
		settings := a.Settings
		if len(settings) == 0 {
			settings = json.RawMessage(`{}`)
		}
		input := dominoaction.New().RuleID(id).Position(i).Name(a.Name).Settings(settings)
		if _, err := dominoaction.Insert(tx, input); err != nil {
			return fmt.Errorf("pgdomino: inserting action %d: %w", i, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgdomino: unable to commit: %w", err)
	}
	r.ID = strconv.FormatInt(id, 10)
	return nil
}

// saveRule inserts or updates the rule's row and returns its ID
func saveRule(tx pgx.Tx, ownerID string, r *domino.Rule) (int64, error) {
	if r.ID == "" {
		row, err := dominorule.Insert(tx, dominorule.New().OwnerID(ownerID).Name(r.Name).Trigger(r.Trigger))
		if err != nil {
			return 0, fmt.Errorf("pgdomino: inserting rule: %w", err)
		}
		return row.ID, nil
	}
	id, err := strconv.ParseInt(r.ID, 10, 64)
	if err != nil {
		return 0, domino.ErrNotFound
	}
	rows, err := dominorule.UpdateMany(tx,
		dominorule.New().Name(r.Name).Trigger(r.Trigger).UpdatedAt(time.Now()),
		dominorule.NewFilter().ID(id).OwnerID(ownerID),
	)
	if err != nil {
		return 0, fmt.Errorf("pgdomino: updating rule: %w", err)
	}
	if len(rows) == 0 {
		return 0, domino.ErrNotFound
	}
	return id, nil
}

func (s *store) Delete(ctx context.Context, ownerID, id string) error {
	ruleID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return domino.ErrNotFound
	}
	// conditions and actions cascade
	rows, err := dominorule.DeleteMany(s.db, dominorule.NewFilter().ID(ruleID).OwnerID(ownerID))
	if err != nil {
		return fmt.Errorf("pgdomino: deleting rule: %w", err)
	}
	if len(rows) == 0 {
		return domino.ErrNotFound
	}
	return nil
}
