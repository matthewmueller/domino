package pgdomino_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/matryer/is"
	"github.com/matthewmueller/domino"
	"github.com/matthewmueller/domino/pgdomino"
)

func databaseURL() string {
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		return url
	}
	return "postgres://localhost:5432/domino_test?sslmode=disable"
}

// connect opens a raw connection, skipping the test if Postgres isn't running
func connect(t testing.TB) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, databaseURL())
	if err != nil {
		t.Skipf("unable to connect to %s: %v", databaseURL(), err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	return conn
}

type PullRequestEvent struct {
	Action      string `json:"action"`
	PullRequest struct {
		Title string `json:"title"`
	} `json:"pull_request"`
}

type CreateTask struct {
	Lane  string `json:"lane"`
	Title string `json:"title"`
}

// dial connects an engine to an empty domino schema with a github trigger and
// an action that records created tasks
func dial(t testing.TB, tasks *[]*CreateTask) *pgdomino.Engine {
	t.Helper()
	conn := connect(t)
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(t.Output(), nil))
	engine, err := pgdomino.Dial(ctx, log, databaseURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Close)
	if _, err := conn.Exec(ctx, `truncate domino_rules cascade`); err != nil {
		t.Fatal(err)
	}
	engine.Trigger("github.pull_request.opened", func(e *PullRequestEvent) bool { return e.Action == "opened" })
	engine.Action("task.create", func(ctx context.Context, in *CreateTask) error {
		*tasks = append(*tasks, in)
		return nil
	})
	return engine
}

func decodeRule(t testing.TB, data string) *domino.Rule {
	t.Helper()
	rule := new(domino.Rule)
	if err := json.Unmarshal([]byte(data), rule); err != nil {
		t.Fatal(err)
	}
	return rule
}

// normalize re-encodes v through a map so JSON key order doesn't matter
func normalize(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	b, err = json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const reviewRule = `{"name": "review", "trigger": "github.pull_request.opened",
	"conditions": [
		{"field": "pull_request.title", "op": "not_contains", "value": "WIP"},
		{"field": "action", "op": "in", "value": ["opened", "reopened"]}],
	"actions": [
		{"name": "task.create", "settings": {"title": "Review: {pull_request.title}", "lane": "review"}},
		{"name": "task.create", "settings": {"lane": "audit", "title": "Audit"}}]}`

func TestSaveList(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	var tasks []*CreateTask
	engine := dial(t, &tasks)
	rule := decodeRule(t, reviewRule)
	is.NoErr(engine.Save(ctx, "acme", rule))
	is.True(rule.ID != "")
	rules, err := engine.List(ctx, "acme")
	is.NoErr(err)
	is.Equal(len(rules), 1)
	is.Equal(normalize(t, rules[0]), normalize(t, rule))
	// only the requested triggers' rules are loaded
	rules, err = engine.List(ctx, "acme", "github.pull_request.opened", "slack.message")
	is.NoErr(err)
	is.Equal(len(rules), 1)
	rules, err = engine.List(ctx, "acme", "slack.message")
	is.NoErr(err)
	is.Equal(len(rules), 0)
}

func TestUpdate(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	var tasks []*CreateTask
	engine := dial(t, &tasks)
	rule := decodeRule(t, reviewRule)
	is.NoErr(engine.Save(ctx, "acme", rule))
	id := rule.ID
	rule.Name = "renamed"
	rule.Conditions = rule.Conditions[1:]
	rule.Actions = rule.Actions[:1]
	is.NoErr(engine.Save(ctx, "acme", rule))
	is.Equal(rule.ID, id)
	rules, err := engine.List(ctx, "acme")
	is.NoErr(err)
	is.Equal(len(rules), 1)
	is.Equal(normalize(t, rules[0]), normalize(t, rule))
}

func TestScope(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	var tasks []*CreateTask
	engine := dial(t, &tasks)
	rule := decodeRule(t, reviewRule)
	is.NoErr(engine.Save(ctx, "acme", rule))

	rules, err := engine.List(ctx, "globex")
	is.NoErr(err)
	is.Equal(len(rules), 0)

	is.True(errors.Is(engine.Save(ctx, "globex", rule), domino.ErrNotFound))
	is.True(errors.Is(engine.Delete(ctx, "globex", rule.ID), domino.ErrNotFound))
	is.True(errors.Is(engine.Delete(ctx, "acme", "nope"), domino.ErrNotFound))
	missing := decodeRule(t, reviewRule)
	missing.ID = "nope"
	is.True(errors.Is(engine.Save(ctx, "acme", missing), domino.ErrNotFound))
}

func TestSaveInvalid(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	var tasks []*CreateTask
	engine := dial(t, &tasks)
	rule := decodeRule(t, `{"name": "r", "trigger": "github.pull_request.opened",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "{pull_request.titel}"}}]}`)
	err := engine.Save(ctx, "acme", rule)
	is.Equal(err.Error(), `actions[0].settings.title: unknown variable "pull_request.titel"`)
	rules, err := engine.List(ctx, "acme")
	is.NoErr(err)
	is.Equal(len(rules), 0)
}

func TestDelete(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	var tasks []*CreateTask
	engine := dial(t, &tasks)
	rule := decodeRule(t, reviewRule)
	is.NoErr(engine.Save(ctx, "acme", rule))
	is.NoErr(engine.Delete(ctx, "acme", rule.ID))
	rules, err := engine.List(ctx, "acme")
	is.NoErr(err)
	is.Equal(len(rules), 0)
	var steps int
	is.NoErr(connect(t).QueryRow(ctx, `select (select count(*) from domino_conditions) + (select count(*) from domino_actions)`).Scan(&steps))
	is.Equal(steps, 0)
}

func TestDispatch(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	var tasks []*CreateTask
	engine := dial(t, &tasks)
	is.NoErr(engine.Save(ctx, "acme", decodeRule(t, reviewRule)))
	event := &PullRequestEvent{Action: "opened"}
	event.PullRequest.Title = "Fix login"
	is.NoErr(engine.Dispatch(ctx, "acme", event))
	is.Equal(len(tasks), 2)
	is.Equal(*tasks[0], CreateTask{Lane: "review", Title: "Review: Fix login"})
	is.Equal(*tasks[1], CreateTask{Lane: "audit", Title: "Audit"})
	// other owners have no rules
	is.NoErr(engine.Dispatch(ctx, "globex", event))
	is.Equal(len(tasks), 2)
}

func TestDialTwice(t *testing.T) {
	is := is.New(t)
	connect(t)
	ctx := context.Background()
	for range 2 {
		engine, err := pgdomino.Dial(ctx, slog.New(slog.DiscardHandler), databaseURL())
		is.NoErr(err)
		engine.Close()
	}
}
