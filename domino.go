// Package domino runs user-built rules: when a trigger fires and every
// condition holds, run each action. Developers define the triggers and actions
// in Go; users compose them into rules that are stored and validated as data.
package domino

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// New creates an engine with an in-memory Store
func New() *Engine {
	return &Engine{
		Store:    &memoryStore{},
		triggers: map[string]*trigger{},
		byEvent:  map[reflect.Type][]*trigger{},
		actions:  map[string]*action{},
	}
}

// Engine validates, stores and runs rules against the triggers and actions
// defined on it. Define everything during initialization, after which it's
// safe for concurrent use.
type Engine struct {
	Store Store // where rules are kept, in memory by default; replace before use

	triggers map[string]*trigger
	byEvent  map[reflect.Type][]*trigger
	actions  map[string]*action
}

// Store persists each owner's rules
type Store interface {
	// Save inserts the rule when its ID is empty, setting the ID, and updates it
	// otherwise. Updating a rule that doesn't exist for the owner returns
	// ErrNotFound.
	Save(ctx context.Context, owner string, rule *Rule) error
	// List returns the owner's rules in the order they were created. When
	// triggers are given, only rules on those triggers are needed; Dispatch
	// passes the triggers an event fired so stores can load less.
	List(ctx context.Context, owner string, triggers ...string) ([]*Rule, error)
	// Delete removes a rule, returning ErrNotFound if it doesn't exist for the
	// owner
	Delete(ctx context.Context, owner, id string) error
}

// ErrNotFound is returned by a Store when a rule doesn't exist for the owner
var ErrNotFound = errors.New("domino: rule not found")

// Rule runs its actions when its trigger fires and every condition holds
type Rule struct {
	ID         string       `json:"id,omitempty"`
	Name       string       `json:"name"`
	Trigger    string       `json:"trigger"` // e.g. github.pull_request.opened
	Conditions []*Condition `json:"conditions,omitempty"`
	Actions    []*Action    `json:"actions"`
}

// Condition compares a field of the event's JSON with a value. contains,
// not_contains, starts_with and ends_with ignore case.
type Condition struct {
	Field string `json:"field"` // dotted JSON path, e.g. "pull_request.base.ref"
	Op    string `json:"op"`    // =, !=, >, <, >=, <=, in, not_in, contains, not_contains, starts_with, ends_with
	Value any    `json:"value"` // a list for in and not_in
}

// Action configures an action defined on the engine
type Action struct {
	Name     string          `json:"name"`
	Settings json.RawMessage `json:"settings,omitempty"`
}

type trigger struct {
	name   string
	match  func(event any) bool
	sample any
}

type action struct {
	check func(settings []byte, sample any) error
	run   func(ctx context.Context, settings []byte, data any) error
}

// Trigger defines an event a rule can start from, named by source and event,
// e.g. "github.pull_request.opened". Without match, every value of type E
// fires it:
//
//	engine.Trigger[slack.Message]("slack.message")
//
// match narrows a payload type shared by several events, e.g. GitHub sends
// opened and closed as one PullRequestEvent. It panics if the name is already
// defined.
func (e *Engine) Trigger[E any](name string, match ...func(event *E) bool) {
	if _, ok := e.triggers[name]; ok {
		panic(fmt.Sprintf("domino: trigger %q already defined", name))
	}
	typ := reflect.TypeFor[E]()
	t := &trigger{
		name: name,
		match: func(event any) bool {
			for _, fn := range match {
				if !fn(event.(*E)) {
					return false
				}
			}
			return true
		},
		sample: sample(typ, map[reflect.Type]bool{}),
	}
	e.triggers[name] = t
	e.byEvent[typ] = append(e.byEvent[typ], t)
}

// Action defines something a rule can do. A rule's settings are decoded into
// S, rejecting unknown fields, and S's Validate() error method is called if it
// has one. String settings can include {variables} from the event's JSON, e.g.
// "Review: {pull_request.title}", which are filled in before decoding. It
// panics if the name is already defined.
func (e *Engine) Action[S any](name string, fn func(ctx context.Context, settings *S) error) {
	if _, ok := e.actions[name]; ok {
		panic(fmt.Sprintf("domino: action %q already defined", name))
	}
	e.actions[name] = &action{
		check: func(settings []byte, sample any) error {
			if _, err := decode[S](settings); err != nil {
				return fmt.Errorf("settings: %w", err)
			}
			return checkTemplates(settings, sample)
		},
		run: func(ctx context.Context, settings []byte, data any) error {
			rendered, err := renderTemplates(settings, data)
			if err != nil {
				return err
			}
			s, err := decode[S](rendered)
			if err != nil {
				return fmt.Errorf("settings: %w", err)
			}
			return fn(ctx, s)
		},
	}
}

// Validate checks a rule against the engine's triggers and actions. Every
// problem is reported on its own line, prefixed with its path, e.g.
// `conditions[0].field: unknown field "pull_request.titel"`.
func (e *Engine) Validate(rule *Rule) error {
	var errs []error
	if rule.Name == "" {
		errs = append(errs, errors.New("name: required"))
	}
	// fields and templates can only be checked against a known trigger
	t, ok := e.triggers[rule.Trigger]
	if !ok {
		errs = append(errs, fmt.Errorf("trigger: unknown trigger %q", rule.Trigger))
	}
	for i, c := range rule.Conditions {
		if c == nil {
			errs = append(errs, fmt.Errorf("conditions[%d]: required", i))
			continue
		}
		if t == nil {
			continue
		}
		if err := checkCondition(c, t.sample); err != nil {
			errs = append(errs, prefix(fmt.Sprintf("conditions[%d].", i), err))
		}
	}
	if len(rule.Actions) == 0 {
		errs = append(errs, errors.New("actions: required"))
	}
	for i, a := range rule.Actions {
		if a == nil {
			errs = append(errs, fmt.Errorf("actions[%d]: required", i))
			continue
		}
		act, ok := e.actions[a.Name]
		if !ok {
			errs = append(errs, fmt.Errorf("actions[%d].name: unknown action %q", i, a.Name))
			continue
		}
		if t == nil {
			continue
		}
		if err := act.check(a.Settings, t.sample); err != nil {
			errs = append(errs, prefix(fmt.Sprintf("actions[%d].", i), err))
		}
	}
	return errors.Join(errs...)
}

var _ Store = (*Engine)(nil)

// Save validates the rule, then stores it for the owner: inserted when its ID
// is empty, setting the ID, or updated otherwise. Take the owner from the
// authenticated session, not from submitted input.
func (e *Engine) Save(ctx context.Context, owner string, rule *Rule) error {
	if err := e.Validate(rule); err != nil {
		return err
	}
	return e.Store.Save(ctx, owner, rule)
}

// List returns the owner's rules in the order they were created, only those on
// triggers if any are given
func (e *Engine) List(ctx context.Context, owner string, triggers ...string) ([]*Rule, error) {
	return e.Store.List(ctx, owner, triggers...)
}

// Delete removes a rule, returning ErrNotFound if it doesn't exist for the
// owner
func (e *Engine) Delete(ctx context.Context, owner, id string) error {
	return e.Store.Delete(ctx, owner, id)
}

// Dispatch runs the owner's rules whose trigger fires for event, in the order
// the Store lists them. It's an error if no trigger is defined for E. A failing
// rule doesn't stop the others; errors are joined.
func (e *Engine) Dispatch[E any](ctx context.Context, owner string, event *E) error {
	triggers := e.byEvent[reflect.TypeFor[E]()]
	if len(triggers) == 0 {
		return fmt.Errorf("domino: no triggers for %T", event)
	}
	var names []string
	fired := map[string]bool{}
	for _, t := range triggers {
		if t.match(event) {
			names = append(names, t.name)
			fired[t.name] = true
		}
	}
	if len(fired) == 0 {
		return nil
	}
	rules, err := e.Store.List(ctx, owner, names...)
	if err != nil {
		return fmt.Errorf("domino: listing rules: %w", err)
	}
	data, err := toData(event)
	if err != nil {
		return fmt.Errorf("domino: encoding event: %w", err)
	}
	var errs []error
	for _, rule := range rules {
		// the store may return more than the fired triggers' rules
		if !fired[rule.Trigger] {
			continue
		}
		if err := e.run(ctx, rule, data); err != nil {
			errs = append(errs, fmt.Errorf("domino: rule %s %q: %w", rule.ID, rule.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (e *Engine) run(ctx context.Context, rule *Rule, data any) error {
	for i, c := range rule.Conditions {
		ok, err := matches(c, data)
		if err != nil {
			return fmt.Errorf("conditions[%d]: %w", i, err)
		}
		if !ok {
			return nil
		}
	}
	var errs []error
	for i, a := range rule.Actions {
		act, ok := e.actions[a.Name]
		if !ok {
			errs = append(errs, fmt.Errorf("actions[%d]: unknown action %q", i, a.Name))
			continue
		}
		if err := act.run(ctx, a.Settings, data); err != nil {
			errs = append(errs, fmt.Errorf("actions[%d] %s: %w", i, a.Name, err))
		}
	}
	return errors.Join(errs...)
}

// prefix adds p to the start of every joined error
func prefix(p string, err error) error {
	var errs []error
	for line := range strings.SplitSeq(err.Error(), "\n") {
		errs = append(errs, errors.New(p+line))
	}
	return errors.Join(errs...)
}

// decode decodes settings into S, rejecting unknown fields, then validates it
func decode[S any](settings []byte) (*S, error) {
	if len(settings) == 0 {
		settings = []byte("{}")
	}
	s := new(S)
	dec := json.NewDecoder(bytes.NewReader(settings))
	dec.DisallowUnknownFields()
	if err := dec.Decode(s); err != nil {
		return nil, err
	}
	if v, ok := any(s).(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// toData converts an event into its JSON form, without nulls, so missing
// values in conditions and templates behave the same as absent ones
func toData(event any) (any, error) {
	b, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	var data any
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, err
	}
	return prune(data), nil
}

func prune(v any) any {
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			if child == nil {
				delete(v, k)
				continue
			}
			v[k] = prune(child)
		}
	case []any:
		for i, child := range v {
			v[i] = prune(child)
		}
	}
	return v
}

// checkTemplates checks that every {variable} in settings exists on the
// trigger's sample, so misspelled variables are caught when a rule is saved
func checkTemplates(settings []byte, sample any) error {
	_, err := walkSettings(settings, func(s string) (string, error) {
		return expand(s, func(path string) (string, error) {
			v, ok := lookup(sample, strings.Split(path, "."))
			if !ok {
				return "", fmt.Errorf("unknown variable %q", path)
			}
			if kind(v) == "object" {
				return "", fmt.Errorf("variable %q is an object, use one of its fields", path)
			}
			return "", nil
		})
	})
	return err
}

// renderTemplates replaces every {variable} in settings with the event's value
func renderTemplates(settings []byte, data any) ([]byte, error) {
	tree, err := walkSettings(settings, func(s string) (string, error) {
		return expand(s, func(path string) (string, error) {
			v, _ := lookup(data, strings.Split(path, "."))
			return format(v)
		})
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(tree)
}

// expand replaces each {path} in s with fn(path). Braces around anything that
// isn't a dotted path, like "{}" or "{not a path}", are left as they are.
func expand(s string, fn func(path string) (string, error)) (string, error) {
	var sb strings.Builder
	var errs []error
	for {
		open := strings.IndexByte(s, '{')
		if open < 0 {
			break
		}
		end := strings.IndexByte(s[open:], '}')
		if end < 0 {
			break
		}
		end += open
		path := strings.TrimSpace(s[open+1 : end])
		if !isPath(path) {
			// not a variable, keep the brace and keep scanning after it
			sb.WriteString(s[:open+1])
			s = s[open+1:]
			continue
		}
		value, err := fn(path)
		if err != nil {
			errs = append(errs, err)
		}
		sb.WriteString(s[:open])
		sb.WriteString(value)
		s = s[end+1:]
	}
	sb.WriteString(s)
	return sb.String(), errors.Join(errs...)
}

// isPath reports whether s is a dotted path like "pull_request.base.ref"
func isPath(s string) bool {
	if s == "" {
		return false
	}
	for segment := range strings.SplitSeq(s, ".") {
		if segment == "" {
			return false
		}
		for _, r := range segment {
			if r != '_' && r != '-' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				return false
			}
		}
	}
	return true
}

// format renders an event value as text. Missing values are empty and arrays
// are joined with commas.
func format(v any) (string, error) {
	switch v := v.(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	case bool:
		return strconv.FormatBool(v), nil
	case []any:
		parts := make([]string, 0, len(v))
		for _, el := range v {
			if el == nil {
				continue
			}
			part, err := format(el)
			if err != nil {
				return "", err
			}
			parts = append(parts, part)
		}
		return strings.Join(parts, ", "), nil
	default:
		b, err := json.Marshal(v)
		return string(b), err
	}
}

// walkSettings applies fn to every string in settings, reporting errors with
// their path
func walkSettings(settings []byte, fn func(string) (string, error)) (any, error) {
	if len(settings) == 0 {
		return map[string]any{}, nil
	}
	var tree any
	if err := json.Unmarshal(settings, &tree); err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	var errs []error
	tree = walk(tree, "settings", func(path, s string) string {
		out, err := fn(s)
		if err != nil {
			errs = append(errs, prefix(path+": ", err))
		}
		return out
	})
	return tree, errors.Join(errs...)
}

// walk replaces every string in a decoded JSON tree with fn(path, string),
// visiting map keys in sorted order
func walk(v any, path string, fn func(path, s string) string) any {
	switch v := v.(type) {
	case string:
		return fn(path, v)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			v[k] = walk(v[k], path+"."+k, fn)
		}
		return v
	case []any:
		for i, child := range v {
			v[i] = walk(child, fmt.Sprintf("%s[%d]", path, i), fn)
		}
		return v
	default:
		return v
	}
}
