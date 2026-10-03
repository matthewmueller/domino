package domino_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/matryer/is"
	"github.com/matthewmueller/domino"
)

type User struct {
	Login string `json:"login"`
}

type Label struct {
	Name string `json:"name"`
}

type PullRequest struct {
	Title     string `json:"title"`
	HTMLURL   string `json:"html_url"`
	Additions int    `json:"additions"`
	Draft     bool   `json:"draft"`
	Base      struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Labels   []Label `json:"labels"`
	MergedBy *User   `json:"merged_by"`
}

type PullRequestEvent struct {
	Action      string       `json:"action"`
	PullRequest *PullRequest `json:"pull_request"`
}

type SlackMessage struct {
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

type Unhandled struct{}

type CreateTask struct {
	Lane     string `json:"lane"`
	Title    string `json:"title"`
	Ref      string `json:"ref,omitempty"`
	Priority int    `json:"priority,omitempty"`
}

func (c *CreateTask) Validate() error {
	if c.Lane == "" {
		return errors.New("lane is required")
	}
	return nil
}

type Fail struct{}

// newEngine defines the test triggers and actions. Created tasks are appended
// to tasks.
func newEngine(t testing.TB, tasks *[]*CreateTask) *domino.Engine {
	t.Helper()
	var mu sync.Mutex
	engine := domino.New()
	engine.Trigger("github.pull_request.opened", func(e *PullRequestEvent) bool { return e.Action == "opened" })
	engine.Trigger("github.pull_request.closed", func(e *PullRequestEvent) bool { return e.Action == "closed" })
	engine.Trigger[SlackMessage]("slack.message")
	engine.Action("task.create", func(ctx context.Context, in *CreateTask) error {
		mu.Lock()
		defer mu.Unlock()
		*tasks = append(*tasks, in)
		return nil
	})
	engine.Action("fail", func(ctx context.Context, in *Fail) error {
		return errors.New("boom")
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

// save validates and stores a rule for owner "1"
func save(t testing.TB, engine *domino.Engine, data string) *domino.Rule {
	t.Helper()
	rule := decodeRule(t, data)
	if err := engine.Save(context.Background(), "1", rule); err != nil {
		t.Fatal(err)
	}
	return rule
}

func opened(title string) *PullRequestEvent {
	pr := &PullRequest{Title: title, HTMLURL: "https://github.com/acme/app/pull/1", Additions: 10}
	pr.Base.Ref = "main"
	pr.Labels = []Label{{Name: "bug"}, {Name: "ui"}}
	return &PullRequestEvent{Action: "opened", PullRequest: pr}
}

func titles(tasks []*CreateTask) []string {
	out := []string{}
	for _, task := range tasks {
		out = append(out, task.Title)
	}
	return out
}

func TestDispatch(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	save(t, engine, `{"name": "review", "trigger": "github.pull_request.opened",
		"actions": [{"name": "task.create", "settings": {
			"lane": "review", "title": "Review: {pull_request.title}", "ref": "{pull_request.html_url}", "priority": 3}}]}`)
	is.NoErr(engine.Dispatch(context.Background(), "1", opened("Fix login")))
	is.Equal(len(tasks), 1)
	is.Equal(*tasks[0], CreateTask{Lane: "review", Title: "Review: Fix login", Ref: "https://github.com/acme/app/pull/1", Priority: 3})
}

func TestDispatchTriggerMatch(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	save(t, engine, `{"name": "r", "trigger": "github.pull_request.opened",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "t"}}]}`)
	closed := opened("Fix login")
	closed.Action = "closed"
	is.NoErr(engine.Dispatch(context.Background(), "1", closed))
	is.Equal(len(tasks), 0)
}

func TestDispatchOtherOwner(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	save(t, engine, `{"name": "r", "trigger": "github.pull_request.opened",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "t"}}]}`)
	is.NoErr(engine.Dispatch(context.Background(), "2", opened("Fix login")))
	is.Equal(len(tasks), 0)
}

func TestDispatchOtherEvent(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	save(t, engine, `{"name": "pr", "trigger": "github.pull_request.opened",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "pr"}}]}`)
	save(t, engine, `{"name": "slack", "trigger": "slack.message",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "{text}"}}]}`)
	is.NoErr(engine.Dispatch(context.Background(), "1", &SlackMessage{Channel: "bug", Text: "hi"}))
	is.Equal(titles(tasks), []string{"hi"})
}

// failStore fails the test if it's used
type failStore struct{ t testing.TB }

func (s failStore) Save(ctx context.Context, owner string, rule *domino.Rule) error {
	s.t.Fatal("unexpected Save")
	return nil
}

func (s failStore) List(ctx context.Context, owner string, triggers ...string) ([]*domino.Rule, error) {
	s.t.Fatal("unexpected List")
	return nil, nil
}

func (s failStore) Delete(ctx context.Context, owner, id string) error {
	s.t.Fatal("unexpected Delete")
	return nil
}

func TestDispatchNoTrigger(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	engine.Store = failStore{t}
	edited := opened("Fix login")
	edited.Action = "edited"
	is.NoErr(engine.Dispatch(context.Background(), "1", edited))
}

// Triggers that share an event type all fire unless match tells them apart
func TestDispatchSharedType(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	engine.Trigger("slack.bug", func(m *SlackMessage) bool { return m.Channel == "bug" })
	save(t, engine, `{"name": "all", "trigger": "slack.message",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "all"}}]}`)
	save(t, engine, `{"name": "bug", "trigger": "slack.bug",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "bug"}}]}`)
	is.NoErr(engine.Dispatch(context.Background(), "1", &SlackMessage{Channel: "general"}))
	is.Equal(titles(tasks), []string{"all"})
	is.NoErr(engine.Dispatch(context.Background(), "1", &SlackMessage{Channel: "bug"}))
	is.Equal(titles(tasks), []string{"all", "all", "bug"})
}

func TestDispatchUndefined(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	engine.Store = failStore{t}
	err := engine.Dispatch(context.Background(), "1", &Unhandled{})
	is.Equal(err.Error(), `domino: no triggers for *domino_test.Unhandled`)
}

func TestDispatchConditions(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	save(t, engine, `{"name": "r", "trigger": "github.pull_request.opened",
		"conditions": [
			{"field": "pull_request.base.ref", "op": "=", "value": "main"},
			{"field": "pull_request.title", "op": "not_contains", "value": "WIP"}],
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "{pull_request.title}"}}]}`)
	ctx := context.Background()
	is.NoErr(engine.Dispatch(ctx, "1", opened("WIP: Fix login")))
	other := opened("Fix login")
	other.PullRequest.Base.Ref = "dev"
	is.NoErr(engine.Dispatch(ctx, "1", other))
	is.NoErr(engine.Dispatch(ctx, "1", opened("Fix login")))
	is.Equal(titles(tasks), []string{"Fix login"})
}

func TestDispatchOrder(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	save(t, engine, `{"name": "a", "trigger": "slack.message",
		"actions": [
			{"name": "task.create", "settings": {"lane": "l", "title": "a1"}},
			{"name": "task.create", "settings": {"lane": "l", "title": "a2"}}]}`)
	save(t, engine, `{"name": "b", "trigger": "slack.message",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "b1"}}]}`)
	is.NoErr(engine.Dispatch(context.Background(), "1", &SlackMessage{}))
	is.Equal(titles(tasks), []string{"a1", "a2", "b1"})
}

func TestDispatchErrors(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	save(t, engine, `{"name": "a", "trigger": "slack.message",
		"actions": [{"name": "fail"}, {"name": "task.create", "settings": {"lane": "l", "title": "a"}}]}`)
	save(t, engine, `{"name": "b", "trigger": "slack.message",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "b"}}, {"name": "fail"}]}`)
	err := engine.Dispatch(context.Background(), "1", &SlackMessage{})
	is.Equal(titles(tasks), []string{"a", "b"})
	is.Equal(err.Error(), "domino: rule 1 \"a\": actions[0] fail: boom\ndomino: rule 2 \"b\": actions[1] fail: boom")
}

func TestDispatchStaleRule(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	old := newEngine(t, &tasks)
	old.Action("task.archive", func(ctx context.Context, in *struct{}) error { return nil })
	save(t, old, `{"name": "stale", "trigger": "slack.message", "actions": [{"name": "task.archive"}]}`)
	save(t, old, `{"name": "ok", "trigger": "slack.message",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "ok"}}]}`)
	// a new deploy drops task.archive but keeps the same store
	engine := newEngine(t, &tasks)
	engine.Store = old.Store
	err := engine.Dispatch(context.Background(), "1", &SlackMessage{})
	is.Equal(err.Error(), `domino: rule 1 "stale": actions[0]: unknown action "task.archive"`)
	is.Equal(titles(tasks), []string{"ok"})
}

// match saves a rule with one condition and reports whether it fires for the
// opened("Fix login") event
func match(t testing.TB, condition string) bool {
	t.Helper()
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	save(t, engine, `{"name": "r", "trigger": "github.pull_request.opened",
		"conditions": [`+condition+`],
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "t"}}]}`)
	if err := engine.Dispatch(context.Background(), "1", opened("Fix login")); err != nil {
		t.Fatal(err)
	}
	return len(tasks) == 1
}

func TestEquals(t *testing.T) {
	is := is.New(t)
	is.True(match(t, `{"field": "pull_request.title", "op": "=", "value": "Fix login"}`))
	is.True(!match(t, `{"field": "pull_request.title", "op": "=", "value": "fix login"}`))
	is.True(match(t, `{"field": "pull_request.additions", "op": "=", "value": 10}`))
	is.True(match(t, `{"field": "pull_request.draft", "op": "=", "value": false}`))
}

func TestNotEquals(t *testing.T) {
	is := is.New(t)
	is.True(match(t, `{"field": "pull_request.title", "op": "!=", "value": "Other"}`))
	is.True(!match(t, `{"field": "pull_request.title", "op": "!=", "value": "Fix login"}`))
}

func TestGreaterLess(t *testing.T) {
	is := is.New(t)
	is.True(match(t, `{"field": "pull_request.additions", "op": ">", "value": 5}`))
	is.True(!match(t, `{"field": "pull_request.additions", "op": ">", "value": 10}`))
	is.True(match(t, `{"field": "pull_request.additions", "op": "<", "value": 11}`))
	is.True(match(t, `{"field": "pull_request.title", "op": ">", "value": "A"}`))
	is.True(!match(t, `{"field": "pull_request.title", "op": "<", "value": "A"}`))
}

func TestContains(t *testing.T) {
	is := is.New(t)
	is.True(match(t, `{"field": "pull_request.title", "op": "contains", "value": "login"}`))
	is.True(!match(t, `{"field": "pull_request.title", "op": "contains", "value": "logout"}`))
	is.True(match(t, `{"field": "pull_request.labels.name", "op": "contains", "value": "bug"}`))
	is.True(!match(t, `{"field": "pull_request.labels.name", "op": "contains", "value": "docs"}`))
	is.True(match(t, `{"field": "pull_request.labels.name", "op": "not_contains", "value": "docs"}`))
}

func TestGreaterLessEqual(t *testing.T) {
	is := is.New(t)
	is.True(match(t, `{"field": "pull_request.additions", "op": ">=", "value": 10}`))
	is.True(!match(t, `{"field": "pull_request.additions", "op": ">=", "value": 11}`))
	is.True(match(t, `{"field": "pull_request.additions", "op": "<=", "value": 10}`))
	is.True(!match(t, `{"field": "pull_request.additions", "op": "<=", "value": 9.5}`))
	is.True(match(t, `{"field": "pull_request.title", "op": "<=", "value": "Fix login"}`))
}

func TestIn(t *testing.T) {
	is := is.New(t)
	is.True(match(t, `{"field": "pull_request.base.ref", "op": "in", "value": ["dev", "main"]}`))
	is.True(!match(t, `{"field": "pull_request.base.ref", "op": "in", "value": ["dev", "staging"]}`))
	is.True(match(t, `{"field": "pull_request.base.ref", "op": "not_in", "value": ["dev", "staging"]}`))
	is.True(!match(t, `{"field": "pull_request.base.ref", "op": "not_in", "value": ["main"]}`))
	is.True(match(t, `{"field": "pull_request.additions", "op": "in", "value": [1, 10]}`))
	is.True(!match(t, `{"field": "pull_request.base.ref", "op": "in", "value": []}`))
}

func TestStartsEndsWith(t *testing.T) {
	is := is.New(t)
	is.True(match(t, `{"field": "pull_request.title", "op": "starts_with", "value": "Fix"}`))
	is.True(!match(t, `{"field": "pull_request.title", "op": "starts_with", "value": "login"}`))
	is.True(match(t, `{"field": "pull_request.title", "op": "ends_with", "value": "login"}`))
	is.True(!match(t, `{"field": "pull_request.title", "op": "ends_with", "value": "Fix"}`))
}

func TestIgnoreCase(t *testing.T) {
	is := is.New(t)
	is.True(match(t, `{"field": "pull_request.title", "op": "contains", "value": "LOGIN"}`))
	is.True(!match(t, `{"field": "pull_request.title", "op": "not_contains", "value": "LOGIN"}`))
	is.True(match(t, `{"field": "pull_request.title", "op": "starts_with", "value": "fix"}`))
	is.True(match(t, `{"field": "pull_request.title", "op": "ends_with", "value": "Login"}`))
	is.True(match(t, `{"field": "pull_request.labels.name", "op": "contains", "value": "BUG"}`))
	// = and in stay exact
	is.True(!match(t, `{"field": "pull_request.title", "op": "=", "value": "fix login"}`))
	is.True(!match(t, `{"field": "pull_request.base.ref", "op": "in", "value": ["MAIN"]}`))
}

func TestMissingField(t *testing.T) {
	is := is.New(t)
	is.True(match(t, `{"field": "pull_request.merged_by.login", "op": "=", "value": null}`))
	is.True(!match(t, `{"field": "pull_request.merged_by.login", "op": "!=", "value": null}`))
	is.True(!match(t, `{"field": "pull_request.merged_by.login", "op": "contains", "value": "a"}`))
	is.True(match(t, `{"field": "pull_request.merged_by.login", "op": "not_contains", "value": "a"}`))
	is.True(!match(t, `{"field": "pull_request.merged_by.login", "op": ">", "value": "a"}`))
	is.True(!match(t, `{"field": "pull_request.merged_by.login", "op": "starts_with", "value": ""}`))
	is.True(!match(t, `{"field": "pull_request.merged_by.login", "op": "in", "value": ["a"]}`))
	is.True(match(t, `{"field": "pull_request.merged_by.login", "op": "not_in", "value": ["a"]}`))
}

func TestIntValue(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	rule := &domino.Rule{
		Name:       "big",
		Trigger:    "github.pull_request.opened",
		Conditions: []*domino.Condition{{Field: "pull_request.additions", Op: ">", Value: 9}},
		Actions:    []*domino.Action{{Name: "task.create", Settings: json.RawMessage(`{"lane": "l", "title": "big"}`)}},
	}
	is.NoErr(engine.Save(context.Background(), "1", rule))
	is.NoErr(engine.Dispatch(context.Background(), "1", opened("Fix login")))
	is.Equal(titles(tasks), []string{"big"})
}

func TestTemplateMissing(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	save(t, engine, `{"name": "r", "trigger": "github.pull_request.opened",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "[{pull_request.merged_by.login}]"}}]}`)
	is.NoErr(engine.Dispatch(context.Background(), "1", opened("Fix login")))
	is.Equal(titles(tasks), []string{"[]"})
}

func TestTemplateValues(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	save(t, engine, `{"name": "r", "trigger": "github.pull_request.opened",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "{pull_request.labels.name} ({ pull_request.additions }, {pull_request.draft})"}}]}`)
	is.NoErr(engine.Dispatch(context.Background(), "1", opened("Fix login")))
	is.Equal(titles(tasks), []string{"bug, ui (10, false)"})
}

// invalid validates a rule and returns its error message
func invalid(t testing.TB, data string) string {
	t.Helper()
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	err := engine.Validate(decodeRule(t, data))
	if err == nil {
		t.Fatal("expected an error")
	}
	return err.Error()
}

func TestValidateRequired(t *testing.T) {
	is := is.New(t)
	is.Equal(invalid(t, `{}`), "name: required\ntrigger: unknown trigger \"\"\nactions: required")
}

func TestValidateTrigger(t *testing.T) {
	is := is.New(t)
	is.Equal(invalid(t, `{"name": "r", "trigger": "github.pr.opened", "actions": [{"name": "fail"}]}`),
		`trigger: unknown trigger "github.pr.opened"`)
}

func TestValidateNull(t *testing.T) {
	is := is.New(t)
	is.Equal(invalid(t, `{"name": "r", "trigger": "github.pull_request.opened", "conditions": [null], "actions": [null]}`),
		"conditions[0]: required\nactions[0]: required")
}

func TestTriggerMatches(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	engine.Trigger("github.pull_request.opened_big",
		func(e *PullRequestEvent) bool { return e.Action == "opened" },
		func(e *PullRequestEvent) bool { return e.PullRequest.Additions > 100 },
	)
	save(t, engine, `{"name": "r", "trigger": "github.pull_request.opened_big",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "{pull_request.title}"}}]}`)
	ctx := context.Background()
	is.NoErr(engine.Dispatch(ctx, "1", opened("small")))
	big := opened("big")
	big.PullRequest.Additions = 500
	is.NoErr(engine.Dispatch(ctx, "1", big))
	is.Equal(titles(tasks), []string{"big"})
}

func TestValidateUnknownAction(t *testing.T) {
	is := is.New(t)
	is.Equal(invalid(t, `{"name": "r", "trigger": "github.pull_request.opened", "actions": [{"name": "nope"}]}`),
		`actions[0].name: unknown action "nope"`)
}

// invalidCondition validates a rule on the opened trigger with one condition
func invalidCondition(t testing.TB, condition string) string {
	t.Helper()
	return invalid(t, `{"name": "r", "trigger": "github.pull_request.opened",
		"conditions": [`+condition+`],
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "t"}}]}`)
}

func TestValidateConditions(t *testing.T) {
	is := is.New(t)
	is.Equal(invalidCondition(t, `{"field": "pull_request.titel", "op": "=", "value": "x"}`),
		`conditions[0].field: unknown field "pull_request.titel"`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.base", "op": "=", "value": "x"}`),
		`conditions[0].field: "pull_request.base" is an object, compare one of its fields`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.title", "op": "~", "value": "x"}`),
		`conditions[0].op: unknown op "~"`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.additions", "op": "=", "value": "10"}`),
		`conditions[0].value: expected a number`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.draft", "op": ">", "value": true}`),
		`conditions[0].op: ">" needs a number or string field, not boolean`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.labels.name", "op": "contains", "value": 1}`),
		`conditions[0].value: expected a string`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.labels", "op": "contains", "value": "bug"}`),
		`conditions[0].field: "pull_request.labels" is an array of objects, compare one of their fields`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.labels.name", "op": "=", "value": "bug"}`),
		`conditions[0].op: "=" needs a string, number or boolean field, use contains for arrays`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.title", "op": "=", "value": ["x"]}`),
		`conditions[0].value: must be a string, number, boolean or null, not array`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.title", "op": "in", "value": "x"}`),
		`conditions[0].value: expected a list`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.title", "op": "in", "value": ["x", 1]}`),
		`conditions[0].value: expected a string`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.title", "op": "in", "value": [["x"]]}`),
		`conditions[0].value: must be a string, number, boolean or null, not array`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.labels.name", "op": "in", "value": ["bug"]}`),
		`conditions[0].op: "in" needs a string, number or boolean field, use contains for arrays`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.additions", "op": "starts_with", "value": "1"}`),
		`conditions[0].op: "starts_with" needs a string field, not number`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.title", "op": "ends_with", "value": 1}`),
		`conditions[0].value: expected a string`)
	is.Equal(invalidCondition(t, `{"field": "pull_request.draft", "op": ">=", "value": true}`),
		`conditions[0].op: ">=" needs a number or string field, not boolean`)
}

// invalidSettings validates a rule on the opened trigger with task.create settings
func invalidSettings(t testing.TB, settings string) string {
	t.Helper()
	return invalid(t, `{"name": "r", "trigger": "github.pull_request.opened",
		"actions": [{"name": "task.create", "settings": `+settings+`}]}`)
}

func TestValidateSettings(t *testing.T) {
	is := is.New(t)
	is.Equal(invalidSettings(t, `{"lane": "l", "titel": "t"}`),
		`actions[0].settings: json: unknown field "titel"`)
	is.Equal(invalidSettings(t, `{"title": "t"}`),
		`actions[0].settings: lane is required`)
	is.Equal(invalidSettings(t, `{"lane": "l", "title": "{pull_request.base}"}`),
		`actions[0].settings.title: variable "pull_request.base" is an object, use one of its fields`)
	is.Equal(invalidSettings(t, `{"lane": "l", "title": "{pull_request.titel}"}`),
		`actions[0].settings.title: unknown variable "pull_request.titel"`)
	is.Equal(invalidSettings(t, `{"lane": "l", "title": "{pull_request.labels.nam} {pull_request.titel}"}`),
		"actions[0].settings.title: unknown variable \"pull_request.labels.nam\"\nactions[0].settings.title: unknown variable \"pull_request.titel\"")
}

func TestValidateOK(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	is.NoErr(engine.Validate(decodeRule(t, `{"name": "r", "trigger": "github.pull_request.opened",
		"conditions": [
			{"field": "pull_request.merged_by.login", "op": "=", "value": null},
			{"field": "pull_request.labels.name", "op": "contains", "value": "bug"}],
		"actions": [{"name": "task.create", "settings": {
			"lane": "l", "title": "Review: {pull_request.title} by {pull_request.merged_by.login}"}}]}`)))
}

func jsonString(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestStore(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	engine := domino.New()
	rule := decodeRule(t, `{"name": "r", "trigger": "github.t",
		"conditions": [{"field": "a", "op": "=", "value": 1}],
		"actions": [{"name": "a", "settings": {"x": 1}}, {"name": "b"}]}`)
	is.NoErr(engine.Store.Save(ctx, "1", rule))
	is.Equal(rule.ID, "1")
	rules, err := engine.Store.List(ctx, "1")
	is.NoErr(err)
	is.Equal(len(rules), 1)
	is.Equal(jsonString(t, rules[0]), jsonString(t, rule))

	// update, including the trigger
	rule.Name = "renamed"
	rule.Trigger = "slack.message"
	rule.Actions = rule.Actions[:1]
	is.NoErr(engine.Store.Save(ctx, "1", rule))
	rules, err = engine.Store.List(ctx, "1")
	is.NoErr(err)
	is.Equal(len(rules), 1)
	is.Equal(jsonString(t, rules[0]), jsonString(t, rule))

	// stored rules are copies
	rules[0].Name = "changed"
	rules, err = engine.Store.List(ctx, "1")
	is.NoErr(err)
	is.Equal(rules[0].Name, "renamed")

	// other owners can't see, update or delete it
	rules, err = engine.Store.List(ctx, "2")
	is.NoErr(err)
	is.Equal(len(rules), 0)
	is.True(errors.Is(engine.Store.Save(ctx, "2", rule), domino.ErrNotFound))
	is.True(errors.Is(engine.Store.Delete(ctx, "2", rule.ID), domino.ErrNotFound))

	is.NoErr(engine.Store.Delete(ctx, "1", rule.ID))
	rules, err = engine.Store.List(ctx, "1")
	is.NoErr(err)
	is.Equal(len(rules), 0)
	is.True(errors.Is(engine.Store.Delete(ctx, "1", rule.ID), domino.ErrNotFound))
}

func TestConcurrent(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			rule := decodeRule(t, `{"name": "r", "trigger": "slack.message",
				"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "{text}"}}]}`)
			// t.Fatal can't be called from other goroutines, so report with t.Error
			if err := engine.Save(ctx, "1", rule); err != nil {
				t.Error(err)
			}
			if err := engine.Dispatch(ctx, "1", &SlackMessage{Text: "hi"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	is.True(len(tasks) > 0)
}

func TestDuplicate(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	panics := func(fn func()) (msg any) {
		defer func() { msg = recover() }()
		fn()
		return nil
	}
	is.Equal(panics(func() {
		engine.Trigger[SlackMessage]("slack.message")
	}), `domino: trigger "slack.message" already defined`)
	is.Equal(panics(func() {
		engine.Action("fail", func(ctx context.Context, in *Fail) error { return nil })
	}), `domino: action "fail" already defined`)
}

func TestTemplateLiteral(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	save(t, engine, `{"name": "r", "trigger": "slack.message",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "{} {not a variable} {\"a\": 1} {text"}}]}`)
	is.NoErr(engine.Dispatch(context.Background(), "1", &SlackMessage{Text: "hi"}))
	is.Equal(titles(tasks), []string{`{} {not a variable} {"a": 1} {text`})
}

func TestErrorLines(t *testing.T) {
	is := is.New(t)
	msg := invalidSettings(t, `{"lane": "l", "title": "{a}", "ref": "{b}"}`)
	is.Equal(len(strings.Split(msg, "\n")), 2)
}

func TestByteFields(t *testing.T) {
	is := is.New(t)
	type Blob struct {
		Data []byte  `json:"data"` // base64 string
		Sum  [2]byte `json:"sum"`  // array of numbers
	}
	engine := domino.New()
	engine.Trigger[Blob]("test.blob")
	engine.Action("noop", func(ctx context.Context, in *struct{}) error { return nil })
	is.NoErr(engine.Validate(decodeRule(t, `{"name": "r", "trigger": "test.blob",
		"conditions": [
			{"field": "data", "op": "contains", "value": "AQ"},
			{"field": "sum", "op": "contains", "value": 1}],
		"actions": [{"name": "noop"}]}`)))
}

func TestListTriggers(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	save(t, engine, `{"name": "pr", "trigger": "github.pull_request.opened",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "pr"}}]}`)
	save(t, engine, `{"name": "slack", "trigger": "slack.message",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "slack"}}]}`)
	ctx := context.Background()
	names := func(rules []*domino.Rule) (out []string) {
		for _, rule := range rules {
			out = append(out, rule.Name)
		}
		return out
	}
	rules, err := engine.Store.List(ctx, "1")
	is.NoErr(err)
	is.Equal(names(rules), []string{"pr", "slack"})
	rules, err = engine.Store.List(ctx, "1", "slack.message")
	is.NoErr(err)
	is.Equal(names(rules), []string{"slack"})
	rules, err = engine.Store.List(ctx, "1", "github.pull_request.closed")
	is.NoErr(err)
	is.Equal(len(rules), 0)
}

// spyStore records the triggers Dispatch asks for
type spyStore struct {
	domino.Store
	triggers []string
}

func (s *spyStore) List(ctx context.Context, owner string, triggers ...string) ([]*domino.Rule, error) {
	s.triggers = triggers
	return s.Store.List(ctx, owner, triggers...)
}

func TestDispatchListsFired(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	spy := &spyStore{Store: engine.Store}
	engine.Store = spy
	is.NoErr(engine.Dispatch(context.Background(), "1", opened("Fix login")))
	is.Equal(spy.triggers, []string{"github.pull_request.opened"})
	is.NoErr(engine.Dispatch(context.Background(), "1", &SlackMessage{}))
	is.Equal(spy.triggers, []string{"slack.message"})
}

// Settings are validated again after their variables are filled in
func TestValidateOnRun(t *testing.T) {
	is := is.New(t)
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	save(t, engine, `{"name": "r", "trigger": "slack.message",
		"actions": [{"name": "task.create", "settings": {"lane": "{channel}", "title": "t"}}]}`)
	err := engine.Dispatch(context.Background(), "1", &SlackMessage{Channel: ""})
	is.Equal(err.Error(), `domino: rule 1 "r": actions[0] task.create: settings: lane is required`)
	is.Equal(len(tasks), 0)
}

func TestSave(t *testing.T) {
	is := is.New(t)
	ctx := context.Background()
	var tasks []*CreateTask
	engine := newEngine(t, &tasks)
	invalid := decodeRule(t, `{"name": "r", "trigger": "slack.message",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "{txt}"}}]}`)
	err := engine.Save(ctx, "1", invalid)
	is.Equal(err.Error(), `actions[0].settings.title: unknown variable "txt"`)
	is.Equal(invalid.ID, "")
	rules, err := engine.List(ctx, "1")
	is.NoErr(err)
	is.Equal(len(rules), 0)

	valid := decodeRule(t, `{"name": "r", "trigger": "slack.message",
		"actions": [{"name": "task.create", "settings": {"lane": "l", "title": "{text}"}}]}`)
	is.NoErr(engine.Save(ctx, "1", valid))
	rules, err = engine.List(ctx, "1", "slack.message")
	is.NoErr(err)
	is.Equal(len(rules), 1)
	is.NoErr(engine.Delete(ctx, "1", valid.ID))
	is.True(errors.Is(engine.Delete(ctx, "1", valid.ID), domino.ErrNotFound))
}
