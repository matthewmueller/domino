package domino

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// unknown marks a part of the sample whose shape can't be known from the Go
// type (maps and interfaces), so anything beneath it is accepted
type unknown struct{}

// sample builds a value shaped like t's JSON encoding: "" for strings, 0 for
// numbers, false for bools, maps for structs and one-element slices for
// arrays. It's used to check condition fields and templates against the
// trigger's event.
func sample(t reflect.Type, seen map[reflect.Type]bool) any {
	if marshals(t) {
		return ""
	}
	switch t.Kind() {
	case reflect.Pointer:
		return sample(t.Elem(), seen)
	case reflect.String:
		return ""
	case reflect.Bool:
		return false
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return 0.0
	case reflect.Slice, reflect.Array:
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			return "" // []byte encodes as a base64 string
		}
		return []any{sample(t.Elem(), seen)}
	case reflect.Struct:
		if seen[t] {
			return unknown{}
		}
		seen[t] = true
		defer delete(seen, t)
		fields := map[string]any{}
		sampleFields(fields, t, seen)
		return fields
	default:
		return unknown{}
	}
}

// sampleFields adds t's fields to m using encoding/json's naming rules
func sampleFields(m map[string]any, t reflect.Type, seen map[reflect.Type]bool) {
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.Tag.Get("json") == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct && !marshals(ft) {
				sampleFields(m, ft, seen)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		m[name] = sample(f.Type, seen)
	}
}

// marshals reports whether t encodes itself, e.g. time.Time
func marshals(t reflect.Type) bool {
	jsonMarshaler := reflect.TypeFor[json.Marshaler]()
	textMarshaler := reflect.TypeFor[encoding.TextMarshaler]()
	pt := reflect.PointerTo(t)
	return t.Implements(jsonMarshaler) || pt.Implements(jsonMarshaler) ||
		t.Implements(textMarshaler) || pt.Implements(textMarshaler)
}

// lookup follows a dotted path through JSON data. Arrays are plucked, so
// "labels.name" returns every label's name. ok reports whether the path exists.
func lookup(v any, path []string) (value any, ok bool) {
	if len(path) == 0 {
		return v, true
	}
	switch v := v.(type) {
	case map[string]any:
		child, ok := v[path[0]]
		if !ok {
			return nil, false
		}
		return lookup(child, path[1:])
	case []any:
		out := make([]any, 0, len(v))
		ok := len(v) == 0
		for _, el := range v {
			value, found := lookup(el, path)
			out = append(out, value)
			ok = ok || found
		}
		return out, ok
	case unknown:
		return v, true
	default:
		return nil, false
	}
}

// normalize converts a value to its JSON form, so ints compare equal to the
// float64s decoded from events
func normalize(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func kind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

// checkCondition reports whether a condition makes sense for the trigger's
// sample: the field exists, the op is known and the value fits both
func checkCondition(c *Condition, sample any) error {
	field, ok := lookup(sample, strings.Split(c.Field, "."))
	if c.Field == "" || !ok {
		return fmt.Errorf("field: unknown field %q", c.Field)
	}
	if kind(field) == "object" {
		return fmt.Errorf("field: %q is an object, compare one of its fields", c.Field)
	}
	if err := checkOp(c.Op); err != nil {
		return err
	}
	value, err := normalize(c.Value)
	if err != nil {
		return fmt.Errorf("value: %w", err)
	}
	// in and not_in take a list of values, every other op takes one
	values := []any{value}
	if c.Op == "in" || c.Op == "not_in" {
		list, ok := value.([]any)
		if !ok {
			return errors.New("value: expected a list")
		}
		values = list
	}
	for _, v := range values {
		if k := kind(v); k == "array" || k == "object" {
			return fmt.Errorf("value: must be a string, number, boolean or null, not %s", k)
		}
	}
	if _, ok := field.(unknown); ok {
		return nil
	}
	switch c.Op {
	case "=", "!=", "in", "not_in":
		if kind(field) == "array" {
			return fmt.Errorf("op: %q needs a string, number or boolean field, use contains for arrays", c.Op)
		}
		for _, v := range values {
			if v != nil && kind(v) != kind(field) {
				return fmt.Errorf("value: expected a %s", kind(field))
			}
		}
	case ">", "<", ">=", "<=":
		if k := kind(field); k != "number" && k != "string" {
			return fmt.Errorf("op: %q needs a number or string field, not %s", c.Op, k)
		}
		if kind(value) != kind(field) {
			return fmt.Errorf("value: expected a %s", kind(field))
		}
	case "starts_with", "ends_with":
		if k := kind(field); k != "string" {
			return fmt.Errorf("op: %q needs a string field, not %s", c.Op, k)
		}
		if kind(value) != "string" {
			return errors.New("value: expected a string")
		}
	case "contains", "not_contains":
		switch field := field.(type) {
		case string:
			if kind(value) != "string" {
				return errors.New("value: expected a string")
			}
		case []any:
			elem := field[0]
			if _, ok := elem.(unknown); ok {
				return nil
			}
			if k := kind(elem); k == "array" || k == "object" {
				return fmt.Errorf("field: %q is an array of %ss, compare one of their fields", c.Field, k)
			}
			if kind(value) != kind(elem) {
				return fmt.Errorf("value: expected a %s", kind(elem))
			}
		default:
			return fmt.Errorf("op: %q needs a string or array field, not %s", c.Op, kind(field))
		}
	}
	return nil
}

func checkOp(op string) error {
	switch op {
	case "=", "!=", ">", "<", ">=", "<=", "in", "not_in",
		"contains", "not_contains", "starts_with", "ends_with":
		return nil
	}
	return fmt.Errorf("op: unknown op %q", op)
}

// matches evaluates a condition against an event's data. A missing field is
// nil: it equals null and contains nothing.
func matches(c *Condition, data any) (bool, error) {
	field, _ := lookup(data, strings.Split(c.Field, "."))
	value, err := normalize(c.Value)
	if err != nil {
		return false, err
	}
	switch c.Op {
	case "=":
		return equal(field, value), nil
	case "!=":
		return !equal(field, value), nil
	case ">":
		n, ok := compare(field, value)
		return ok && n > 0, nil
	case "<":
		n, ok := compare(field, value)
		return ok && n < 0, nil
	case ">=":
		n, ok := compare(field, value)
		return ok && n >= 0, nil
	case "<=":
		n, ok := compare(field, value)
		return ok && n <= 0, nil
	case "in":
		return in(field, value), nil
	case "not_in":
		return !in(field, value), nil
	case "starts_with":
		s, ok := field.(string)
		prefix, _ := value.(string)
		return ok && strings.HasPrefix(strings.ToLower(s), strings.ToLower(prefix)), nil
	case "ends_with":
		s, ok := field.(string)
		suffix, _ := value.(string)
		return ok && strings.HasSuffix(strings.ToLower(s), strings.ToLower(suffix)), nil
	case "contains":
		return contains(field, value), nil
	case "not_contains":
		return !contains(field, value), nil
	}
	return false, checkOp(c.Op)
}

func equal(a, b any) bool {
	switch kind(a) {
	case "array", "object":
		return false
	}
	switch kind(b) {
	case "array", "object":
		return false
	}
	return a == b
}

func compare(a, b any) (int, bool) {
	switch a := a.(type) {
	case float64:
		b, ok := b.(float64)
		if !ok {
			return 0, false
		}
		switch {
		case a < b:
			return -1, true
		case a > b:
			return 1, true
		}
		return 0, true
	case string:
		b, ok := b.(string)
		if !ok {
			return 0, false
		}
		return strings.Compare(a, b), true
	}
	return 0, false
}

// in reports whether field equals any value in the list
func in(field, list any) bool {
	values, _ := list.([]any)
	for _, v := range values {
		if equal(field, v) {
			return true
		}
	}
	return false
}

// contains reports whether a string contains a substring or an array contains
// an element, ignoring case
func contains(field, value any) bool {
	switch field := field.(type) {
	case string:
		s, ok := value.(string)
		return ok && strings.Contains(strings.ToLower(field), strings.ToLower(s))
	case []any:
		for _, el := range field {
			if equal(el, value) || equalFold(el, value) {
				return true
			}
		}
	}
	return false
}

func equalFold(a, b any) bool {
	as, ok := a.(string)
	if !ok {
		return false
	}
	bs, ok := b.(string)
	return ok && strings.EqualFold(as, bs)
}
