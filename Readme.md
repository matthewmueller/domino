# domino

A small Go library for user-built automations: _when this event happens, and these conditions hold, do these actions_.

Developers define the triggers and actions in typed Go. Users build rules from them, for example in a Zapier- or Butler-style form. Rules are plain data that domino validates, stores and runs when events arrive.

## Install

```sh
go get github.com/matthewmueller/domino
```

Requires Go 1.27+ for generic methods. No dependencies outside the standard library.

## Example

Define what rules can use during initialization:

```go
engine := domino.New()

// Triggers: events that start a rule, named by source and event. An optional
// match narrows payload types shared by several triggers.
engine.Trigger("github.pull_request.opened", func(e *github.PullRequestEvent) bool {
	return e.GetAction() == "opened"
})
engine.Trigger[slack.Message]("slack.message") // every slack.Message fires it

// Actions: things a rule can do, with typed settings
engine.Action("task.create", tasks.Create) // func(ctx context.Context, in *CreateTask) error
```

```go
type CreateTask struct {
	Lane  string `json:"lane"`
	Title string `json:"title"`
	Ref   string `json:"ref,omitempty"`
}

// Validate is optional and runs when a rule is validated and before each run
func (c *CreateTask) Validate() error {
	if c.Lane == "" {
		return errors.New("lane is required")
	}
	return nil
}
```

A user builds a rule:

```json
{
  "name": "Review new PRs",
  "trigger": "github.pull_request.opened",
  "conditions": [
    { "field": "pull_request.base.ref", "op": "=", "value": "main" },
    { "field": "pull_request.title", "op": "not_contains", "value": "WIP" }
  ],
  "actions": [
    {
      "name": "task.create",
      "settings": {
        "lane": "review",
        "title": "Review: {pull_request.title}",
        "ref": "{pull_request.html_url}"
      }
    }
  ]
}
```

Save it. `Save` validates the rule first:

```go
if err := engine.Save(ctx, "acme", rule); err != nil { // owner from your session, not the form
	// one problem per line, with its path:
	// conditions[0].field: unknown field "pull_request.titel"
	return err
}
```

`engine.List` and `engine.Delete` read and remove an owner's rules, and `engine.Validate` checks a rule without saving it.

When a webhook arrives, dispatch the event to its owner's rules:

```go
err := engine.Dispatch(ctx, "acme", event)
```

## Rules

- **Trigger:** the name of a defined trigger, e.g. `github.pull_request.opened`.
- **Conditions:** compare a field of the event's JSON with a value. All conditions must hold.
- **Actions:** run in order. Strings in `settings` can include `{variables}` from the event's JSON, e.g. `"Review: {pull_request.title}"`. They're filled in before the settings are decoded into the action's type:
  - missing values are empty;
  - numbers and booleans are written out (`10`, `true`);
  - arrays are joined with commas, so `{pull_request.labels.name}` becomes `bug, ui`;
  - braces around anything that isn't a dotted path, like `{}` or `{"a": 1}`, are left as they are.

Fields use the event's JSON names as dotted paths. A path through an array plucks a field from every element, so `pull_request.labels.name` is the list of label names.

| Op                         | Field types             | Matches when                                                                 |
| -------------------------- | ----------------------- | ---------------------------------------------------------------------------- |
| `=`, `!=`                  | string, number, boolean | the field equals or differs from the value. A missing field equals `null`.   |
| `in`, `not_in`             | string, number, boolean | the field equals one of a list of values, e.g. `["bug", "support"]`          |
| `>`, `<`, `>=`, `<=`       | number, string          | numbers compare numerically, strings lexicographically                       |
| `starts_with`, `ends_with` | string                  | the field starts or ends with the value                                      |
| `contains`, `not_contains` | string, array           | substring for strings, element for arrays. A missing field contains nothing. |

`contains`, `not_contains`, `starts_with` and `ends_with` ignore case, so `title contains "wip"` matches "WIP: Fix login". `=`, `!=`, `in` and `not_in` are exact.

`Validate` checks a rule against the trigger's event type:

- the field exists and the operator and value fit its type;
- settings decode into the action's type, with unknown fields rejected;
- `{variables}` only reference fields that exist, and not whole objects.

Map and interface fields can't be checked ahead of time. Conditions and variables on them are accepted as-is.

`Dispatch` runs the owner's rules whose trigger fires for the event. It passes the fired triggers to `Store.List`, so a store only needs to load those rules: dispatching a Slack message loads the owner's `slack.message` rules, not everything. It returns an error if no trigger is defined for the event's type, and reports each failing rule (including one that refers to a trigger or action that no longer exists) without stopping the others, and joins the errors.

## Storage

Rules belong to an owner: an opaque ID that links them to your app, such as an org, user or workspace. Take it from the authenticated session, not from the submitted rule.

Rules are stored in memory by default. To persist them, set `engine.Store` before use to your own implementation. The engine itself implements `Store`, adding validation on `Save`:

```go
type Store interface {
	Save(ctx context.Context, owner string, rule *Rule) error // inserts when ID is "", otherwise updates
	List(ctx context.Context, owner string, triggers ...string) ([]*Rule, error) // in creation order
	Delete(ctx context.Context, owner, id string) error
}
```

Updating or deleting a rule that doesn't exist for the owner returns `domino.ErrNotFound`.

### Postgres

`pgdomino` is a domino engine that stores rules in Postgres. `Dial` connects and runs its migrations:

```go
engine, err := pgdomino.Dial(ctx, log, os.Getenv("DATABASE_URL"))
if err != nil {
	return err
}
defer engine.Close()

// define triggers and actions as usual
engine.Trigger("github.pull_request.opened", func(e *github.PullRequestEvent) bool { return e.GetAction() == "opened" })
engine.Action("task.create", tasks.Create)

err = engine.Save(ctx, org.ID, rule)  // validates, then stores
rules, err := engine.List(ctx, org.ID)
err = engine.Delete(ctx, org.ID, rule.ID)
err = engine.Dispatch(ctx, org.ID, event)
```

Rules live in `domino_rules`, with their conditions and actions in `domino_conditions` and `domino_actions`. Migrations are tracked in `domino_migrate`, so they don't collide with your app's own.

## Development

```sh
createdb domino_test   # for pgdomino's tests
make test
```

After changing a migration in `pgdomino/internal/migrate`, regenerate the database client with `make pogo` (needs a `domino_dev` database and `goimports`).
