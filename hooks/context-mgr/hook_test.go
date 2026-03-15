package main

import (
	"context"
	"encoding/json"
	"testing"

	"tag-gateway/hooklib"
)

func TestInit(t *testing.T) {
	h := &contextMgrHook{maxTokens: 180000}
	result := h.Init(nil)

	if result.Name != "context-mgr" {
		t.Errorf("name: got %q, want %q", result.Name, "context-mgr")
	}
	if _, ok := result.Phases[hooklib.PhasePreReq]; !ok {
		t.Error("missing PRE_REQ phase")
	}
	if result.Phases[hooklib.PhasePreReq].Mode != hooklib.ModeBodyMutate {
		t.Error("PRE_REQ should be BODY_MUTATE")
	}
	if _, ok := result.Phases[hooklib.PhasePostResp]; !ok {
		t.Error("missing POST_RESP phase")
	}
}

func TestProcess_InjectsHistory(t *testing.T) {
	h := &contextMgrHook{maxTokens: 180000}

	body := `{"model":"claude","messages":[{"role":"user","content":"hello"}]}`
	params := &hooklib.ProcessParams{
		Phase: hooklib.PhasePreReq,
		Body:  json.RawMessage(body),
		Metadata: map[string]interface{}{
			"session_key": "test-session",
			"model":       "claude",
			"hook.session-hook.history": []interface{}{
				map[string]interface{}{"role": "user", "content": "old question"},
				map[string]interface{}{"role": "assistant", "content": "old answer"},
			},
			"hook.session-hook.turn_count": float64(2),
		},
	}

	result := h.Process(context.TODO(), params)
	if result.Action != hooklib.ActionContinue {
		t.Fatalf("action: got %q, want CONTINUE", result.Action)
	}

	// Parse the modified body.
	var newBody map[string]interface{}
	if err := json.Unmarshal(result.Body, &newBody); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}

	messages, _ := newBody["messages"].([]interface{})
	// Should be: old question, old answer, hello (no system in this case)
	if len(messages) != 3 {
		t.Fatalf("expected 3 messages, got %d: %v", len(messages), messages)
	}

	// First message should be history (old question)
	msg0, _ := messages[0].(map[string]interface{})
	if msg0["content"] != "old question" {
		t.Errorf("msg[0] content: got %v, want 'old question'", msg0["content"])
	}

	// Last message should be the user's new message
	msgLast, _ := messages[2].(map[string]interface{})
	if msgLast["content"] != "hello" {
		t.Errorf("last msg content: got %v, want 'hello'", msgLast["content"])
	}
}

func TestProcess_PreservesSystemMessage(t *testing.T) {
	h := &contextMgrHook{maxTokens: 180000}

	body := `{"model":"claude","messages":[{"role":"system","content":"you are an agent"},{"role":"user","content":"hi"}]}`
	params := &hooklib.ProcessParams{
		Phase: hooklib.PhasePreReq,
		Body:  json.RawMessage(body),
		Metadata: map[string]interface{}{
			"hook.session-hook.history": []interface{}{
				map[string]interface{}{"role": "user", "content": "prev"},
				map[string]interface{}{"role": "assistant", "content": "resp"},
			},
		},
	}

	result := h.Process(context.TODO(), params)
	var newBody map[string]interface{}
	json.Unmarshal(result.Body, &newBody)
	messages, _ := newBody["messages"].([]interface{})

	// Should be: system, prev, resp, hi
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(messages))
	}

	msg0, _ := messages[0].(map[string]interface{})
	if msg0["role"] != "system" {
		t.Errorf("first message should be system, got %v", msg0["role"])
	}
}

func TestProcess_NoHistory(t *testing.T) {
	h := &contextMgrHook{maxTokens: 180000}

	body := `{"model":"claude","messages":[{"role":"user","content":"first message"}]}`
	params := &hooklib.ProcessParams{
		Phase: hooklib.PhasePreReq,
		Body:  json.RawMessage(body),
		Metadata: map[string]interface{}{
			"hook.session-hook.history":    []interface{}{},
			"hook.session-hook.turn_count": float64(0),
		},
	}

	result := h.Process(context.TODO(), params)
	var newBody map[string]interface{}
	json.Unmarshal(result.Body, &newBody)
	messages, _ := newBody["messages"].([]interface{})

	// Just the user message, no history to inject.
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
}

func TestProcess_TruncatesWhenOverBudget(t *testing.T) {
	h := &contextMgrHook{maxTokens: 200} // very small budget

	// Build a big history that exceeds budget.
	var history []interface{}
	for i := 0; i < 20; i++ {
		history = append(history, map[string]interface{}{
			"role": "user", "content": "message number " + string(rune('A'+i)),
		})
	}

	body := `{"model":"claude","messages":[{"role":"system","content":"sys"},{"role":"user","content":"latest"}]}`
	params := &hooklib.ProcessParams{
		Phase: hooklib.PhasePreReq,
		Body:  json.RawMessage(body),
		Metadata: map[string]interface{}{
			"hook.session-hook.history": history,
		},
	}

	result := h.Process(context.TODO(), params)
	var newBody map[string]interface{}
	json.Unmarshal(result.Body, &newBody)
	messages, _ := newBody["messages"].([]interface{})

	// Should have fewer messages than 22 (system + 20 history + user).
	if len(messages) >= 22 {
		t.Errorf("expected truncation, got %d messages", len(messages))
	}
	// Must still have system (first) and user (last).
	if len(messages) < 2 {
		t.Fatal("truncated too aggressively")
	}
	first, _ := messages[0].(map[string]interface{})
	last, _ := messages[len(messages)-1].(map[string]interface{})
	if first["role"] != "system" {
		t.Errorf("first should be system, got %v", first["role"])
	}
	if last["content"] != "latest" {
		t.Errorf("last should be 'latest', got %v", last["content"])
	}
}

func TestProcess_EmitsSaveMessages(t *testing.T) {
	h := &contextMgrHook{maxTokens: 180000}

	body := `{"model":"claude","messages":[{"role":"user","content":"save this"}]}`
	params := &hooklib.ProcessParams{
		Phase:    hooklib.PhasePreReq,
		Body:     json.RawMessage(body),
		Metadata: map[string]interface{}{},
	}

	result := h.Process(context.TODO(), params)
	if result.MetadataPatch == nil {
		t.Fatal("expected metadata_patch with save_messages")
	}
	sm, ok := result.MetadataPatch["save_messages"]
	if !ok {
		t.Fatal("save_messages not in patch")
	}
	msgs, ok := sm.([]json.RawMessage)
	if !ok || len(msgs) != 1 {
		t.Fatalf("expected 1 save_message, got %v", sm)
	}
}

func TestProcess_SkipsNonPreReq(t *testing.T) {
	h := &contextMgrHook{maxTokens: 180000}

	result := h.Process(context.TODO(), &hooklib.ProcessParams{
		Phase: hooklib.PhasePreResp,
	})
	if result.Action != hooklib.ActionPass {
		t.Errorf("non-PRE_REQ should PASS, got %q", result.Action)
	}
}

func TestExtractHistory_Nil(t *testing.T) {
	h := extractHistory(map[string]interface{}{})
	if h != nil {
		t.Errorf("expected nil, got %v", h)
	}
}

func TestExtractReactTrace(t *testing.T) {
	meta := map[string]interface{}{
		"react_trace": map[string]interface{}{
			"turns": float64(2),
			"steps": []interface{}{
				map[string]interface{}{
					"turn": float64(1), "type": "tool_use", "tool": "fs__write_file",
					"tool_use_id": "call_1", "status": "success", "duration_ms": float64(10),
				},
			},
			"total_duration_ms": float64(5000),
		},
	}

	trace := extractReactTrace(meta)
	if trace == nil {
		t.Fatal("expected trace, got nil")
	}
	if trace.Turns != 2 {
		t.Errorf("turns: got %d, want 2", trace.Turns)
	}
	if len(trace.Steps) != 1 {
		t.Fatalf("steps: got %d, want 1", len(trace.Steps))
	}
	if trace.Steps[0].Tool != "fs__write_file" {
		t.Errorf("step tool: got %q", trace.Steps[0].Tool)
	}
}

func TestEstimateTokens(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "hello world"},
	}
	tokens := estimateTokens(msgs)
	if tokens < 5 || tokens > 100 {
		t.Errorf("token estimate out of range: %d", tokens)
	}
}
