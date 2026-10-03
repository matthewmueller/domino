package domino_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/matthewmueller/domino"
)

func Example() {
	type Message struct {
		Channel string `json:"channel"`
		Text    string `json:"text"`
	}
	type CreateTask struct {
		Lane  string `json:"lane"`
		Title string `json:"title"`
	}

	// Define what rules can use
	engine := domino.New()
	engine.Trigger[Message]("slack.message")
	engine.Action("task.create", func(ctx context.Context, in *CreateTask) error {
		fmt.Printf("%s: %s\n", in.Lane, in.Title)
		return nil
	})

	// A rule built by a user, e.g. from a form
	rule := new(domino.Rule)
	err := json.Unmarshal([]byte(`{
		"name": "Bug intake",
		"trigger": "slack.message",
		"conditions": [{"field": "channel", "op": "=", "value": "bug"}],
		"actions": [{"name": "task.create", "settings": {"lane": "triage", "title": "Bug: {text}"}}]
	}`), rule)
	if err != nil {
		panic(err)
	}

	ctx := context.Background()
	if err := engine.Save(ctx, "acme", rule); err != nil {
		panic(err)
	}

	// Events from a webhook
	engine.Dispatch(ctx, "acme", &Message{Channel: "general", Text: "Lunch?"})
	engine.Dispatch(ctx, "acme", &Message{Channel: "bug", Text: "Login is broken"})
	// Output: triage: Bug: Login is broken
}
