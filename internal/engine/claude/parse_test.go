package claude

import (
	"encoding/json"
	"testing"

	"github.com/viminizer/germes/internal/engine"
)

func TestParseTurn(t *testing.T) {
	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s1"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"o"}}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"echo hi"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"hi","is_error":false}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":100,"unifiedWindows":{"five_hour":{"utilization":1,"resetsAt":100}}}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"ok"}`,
		`not json`,
	}
	var p parser
	var kinds []engine.Kind
	var last engine.Event
	for _, l := range lines {
		for _, ev := range p.parse([]byte(l)) {
			kinds = append(kinds, ev.Kind)
			last = ev
		}
	}
	want := []engine.Kind{engine.KindTextDelta, engine.KindToolCall, engine.KindToolResult, engine.KindText, engine.KindUsage, engine.KindLimitHit, engine.KindDone}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if last.ResumeID != "s1" {
		t.Fatalf("resume id = %q, want s1", last.ResumeID)
	}
}

// A failed result that says the provider is down is an outage the router can
// move on from; any other failed result is an error.
func TestFailedResultKinds(t *testing.T) {
	for result, outage := range map[string]bool{
		`API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`:     true,
		`API Error: Repeated 529 Overloaded errors`:                                                      true,
		`API Error: 500 {"type":"error","error":{"type":"api_error","message":"Internal server error"}}`: true,
		`API Error: Request timed out.`:                                                                  true,
		`API Error: 400 {"type":"error","error":{"message":"bad input"}}`:                                false,
		`Invalid API key · Please run /login`:                                                            false,
	} {
		var p parser
		line, _ := json.Marshal(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": result})
		evs := p.parse(line)
		if len(evs) != 1 || (evs[0].Kind == engine.KindLimitHit && evs[0].Outage) != outage || (!outage && evs[0].Kind != engine.KindError) {
			t.Errorf("%s: got %+v, want outage %v", result, evs, outage)
		}
	}
}

func TestParseSaysHowFullTheContextIs(t *testing.T) {
	var p parser
	for _, l := range []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"a"}],"usage":{"input_tokens":5,"cache_creation_input_tokens":1000,"cache_read_input_tokens":40000}}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"b"}],"usage":{"input_tokens":2,"cache_creation_input_tokens":6749,"cache_read_input_tokens":50506}}}`,
	} {
		p.parse([]byte(l))
	}
	// The result's usage adds up all calls (the cache hit rate counts them
	// all); the last call is how full the context is.
	evs := p.parse([]byte(`{"type":"result","subtype":"success","result":"b",` +
		`"usage":{"input_tokens":7,"cache_creation_input_tokens":7749,"cache_read_input_tokens":90506},` +
		`"modelUsage":{"claude-haiku":{"contextWindow":200000},"claude-opus-5-5":{"contextWindow":1000000}}}`))
	want := engine.Context{Model: "claude-opus-5-5", Used: 57257, Window: 1000000, Input: 98262, Cached: 90506}
	if len(evs) != 1 || evs[0].Context == nil || *evs[0].Context != want {
		t.Fatalf("done %+v", evs)
	}
	var none parser
	if evs := none.parse([]byte(`{"type":"result","subtype":"success","result":"ok"}`)); evs[0].Context != nil {
		t.Fatalf("context without usage: %+v", evs[0].Context)
	}
}
