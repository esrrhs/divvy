package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

// TestParseActionNormalizesAliases covers the many names weak models use for
// "I'm done", plus argument-key and casing normalization.
func TestParseActionNormalizesAliases(t *testing.T) {
	for _, in := range []string{
		`{"action":"done","args":{}}`,
		`{"action":"complete","args":{}}`,
		`{"action":"completed","args":{}}`,
		`{"action":"task_complete","args":{}}`,
		`{"action":"stop","args":{}}`,
		`{"action":"end","args":{}}`,
		`{"action":"DONE","args":{}}`,
	} {
		act, err := ParseAction(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if act.Name != "finish" {
			t.Errorf("%s: name = %q, want finish", in, act.Name)
		}
	}
}

func TestParseActionNormalizesNameSpacingAndCase(t *testing.T) {
	act, err := ParseAction(`{"action":"Read File","args":{"path":"a.go"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if act.Name != "read_file" {
		t.Fatalf("name = %q, want read_file", act.Name)
	}
}

// TestParseActionAcceptsArgumentKeyAliases covers the common key variants for
// the argument object itself.
func TestParseActionAcceptsArgumentKeyAliases(t *testing.T) {
	for _, key := range []string{"args", "arguments", "parameters", "input", "params"} {
		in := `{"action":"write_file","` + key + `":{"path":"a.go"}}`
		act, err := ParseAction(in)
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		if act.Args["path"] != "a.go" {
			t.Errorf("%s: args = %+v, want path=a.go", key, act.Args)
		}
	}
}

// TestParseActionReadsNestedFunctionShape covers the OpenAI-style
// {"function":{"name":..,"arguments":..}} envelope.
func TestParseActionReadsNestedFunctionShape(t *testing.T) {
	act, err := ParseAction(`{"function":{"name":"read_file","arguments":"{\"path\":\"x.go\"}"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if act.Name != "read_file" {
		t.Errorf("name = %q", act.Name)
	}
	if act.Args["path"] != "x.go" {
		t.Errorf("args = %+v", act.Args)
	}
}

func TestParseActionReadsNestedParametersShape(t *testing.T) {
	act, err := ParseAction(`{"function":{"name":"list_dir","parameters":{"path":"."}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if act.Name != "list_dir" {
		t.Errorf("name = %q", act.Name)
	}
	if act.Args["path"] != "." {
		t.Errorf("args = %+v", act.Args)
	}
}

func TestParseActionRequiresAName(t *testing.T) {
	if _, err := ParseAction(`{"thought":"I will do something"}`); err == nil {
		t.Fatal("expected an error when no action name is present")
	}
}

func TestParseActionDefaultsEmptyArgs(t *testing.T) {
	act, err := ParseAction(`{"action":"finish"}`)
	if err != nil {
		t.Fatal(err)
	}
	if act.Args == nil {
		t.Fatal("args should default to an empty map, not nil")
	}
	if len(act.Args) != 0 {
		t.Errorf("args = %+v, want empty", act.Args)
	}
}

func TestParseActionCapturesThoughtAliases(t *testing.T) {
	for _, key := range []string{"thought", "reasoning", "reason", "scratchpad"} {
		act, err := ParseAction(`{"` + key + `":"because","action":"finish"}`)
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		if act.Thought != "because" {
			t.Errorf("%s: thought = %q", key, act.Thought)
		}
	}
}

// TestUnmarshalFlexibleUsesNumbers covers the UseNumber behaviour: large ints
// and exact decimals must survive instead of becoming float64.
func TestUnmarshalFlexibleUsesNumbers(t *testing.T) {
	var dst struct {
		N json.Number `json:"n"`
	}
	if err := UnmarshalFlexible(`{"n":123456789012345678}`, &dst); err != nil {
		t.Fatal(err)
	}
	if dst.N.String() != "123456789012345678" {
		t.Errorf("n = %s, want the exact integer", dst.N)
	}
}

func TestUnmarshalFlexibleReportsBadJSON(t *testing.T) {
	var dst map[string]any
	err := UnmarshalFlexible(`{"a":`, &dst)
	if err == nil {
		t.Fatal("expected an error for truncated JSON")
	}
	if !strings.Contains(err.Error(), "snippet") {
		t.Errorf("error should include a snippet for debugging, got %v", err)
	}
}

func TestUnmarshalFlexibleRejectsNonJSON(t *testing.T) {
	var dst map[string]any
	if err := UnmarshalFlexible("I refuse to answer in JSON.", &dst); err == nil {
		t.Fatal("expected an error when there is no JSON at all")
	}
}

// TestScriptedClientRecordsRequests covers the mock used across the suite.
func TestScriptedClientRecordsRequests(t *testing.T) {
	c := &ScriptedClient{
		Handle: func(ctx context.Context, req Request) (*Response, error) {
			return &Response{Content: "ok"}, nil
		},
	}
	for i := 0; i < 3; i++ {
		if _, err := c.Chat(context.Background(), Request{Model: "m"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(c.Requests) != 3 {
		t.Fatalf("recorded %d requests, want 3", len(c.Requests))
	}
}

// TestScriptedClientWithoutHandler documents that a handler-less client is
// inert rather than panicking.
func TestScriptedClientWithoutHandler(t *testing.T) {
	c := &ScriptedClient{}
	resp, err := c.Chat(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp != nil {
		t.Errorf("resp = %+v, want nil", resp)
	}
}

func TestSequenceClientWalksResponses(t *testing.T) {
	c := &SequenceClient{
		Responses: []*Response{{Content: "first"}, {Content: "second"}},
	}
	for _, want := range []string{"first", "second"} {
		resp, err := c.Chat(context.Background(), Request{})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Content != want {
			t.Errorf("content = %q, want %q", resp.Content, want)
		}
	}
	if c.Index() != 2 {
		t.Errorf("Index = %d, want 2", c.Index())
	}
	// Exhausted: return a harmless finish action instead of failing, so a
	// mis-scripted test degrades rather than erroring.
	resp, err := c.Chat(context.Background(), Request{})
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil || resp.Content == "" {
		t.Fatal("exhausted sequence should still return a response")
	}
}

func TestSequenceClientSurfacesScriptedErrors(t *testing.T) {
	boom := errors.New("boom")
	c := &SequenceClient{Errs: []error{boom}}
	if _, err := c.Chat(context.Background(), Request{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
}

// TestMocksAreConcurrencySafe matters because -parallel tests call Chat from
// several leaf goroutines at once.
func TestMocksAreConcurrencySafe(t *testing.T) {
	scripted := &ScriptedClient{
		Handle: func(ctx context.Context, req Request) (*Response, error) {
			return &Response{Content: "ok"}, nil
		},
	}
	seq := &SequenceClient{Responses: []*Response{{Content: "x"}}}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = scripted.Chat(context.Background(), Request{})
		}()
		go func() {
			defer wg.Done()
			_, _ = seq.Chat(context.Background(), Request{})
		}()
	}
	wg.Wait()

	if len(scripted.Requests) != 50 {
		t.Errorf("scripted recorded %d requests, want 50", len(scripted.Requests))
	}
	if len(seq.Requests) != 50 {
		t.Errorf("sequence recorded %d requests, want 50", len(seq.Requests))
	}
}
