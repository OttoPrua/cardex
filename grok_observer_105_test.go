package main

import (
	"context"
	"strings"
	"testing"
)

// The bounded host probe proved these seven top-level Grok CLI 1.0.5 shapes. It timed out before
// any terminal event, so the final end/end_turn below deliberately remains the older supported
// terminal contract; the standalone signed usage event is not promoted into a terminal.
const grok105ObservedPrefix = `{"commands":[],"tools":[],"type":"available_commands"}` + "\n" +
	`{"data":"planning the answer","type":"thought"}` + "\n" +
	`{"data":"GROK_OK","type":"text"}` + "\n" +
	`{"signature":"opaque-signature","type":"usage","usage":{"input_tokens":12,"cache_read_input_tokens":3,"cache_creation_input_tokens":2,"output_tokens":5}}` + "\n" +
	`{"content":[],"kind":"tool","locations":[],"rawInput":{},"status":"pending","title":"read","toolCallId":"call-1","toolName":"read_file","type":"tool_call"}` + "\n" +
	`{"content":[],"locations":[],"rawOutput":null,"status":null,"toolCallId":"call-1","type":"tool_call_update"}` + "\n" +
	`{"content":[],"locations":[],"rawOutput":{},"status":"completed","toolCallId":"call-1","type":"tool_call_update"}`

const grokLegacyCleanEnd = `{"type":"end","stopReason":"end_turn","sessionId":"session-legacy","num_turns":2,"duration_ms":210}`
const grokLegacyMinimalEnd = `{"type":"end","stopReason":"end_turn","sessionId":"session-legacy"}`

func TestGrokBuild105ObservedShapesWithLegacyTerminalComplete(t *testing.T) {
	res := parseGrokBuildJSONL(grok105ObservedPrefix + "\n" + grokLegacyCleanEnd)
	if res == nil || res.IsError || !res.ObservationComplete {
		t.Fatalf("proved 1.0.5 shapes plus the legacy clean terminal must parse: %+v", res)
	}
	if res.Result != "GROK_OK" || res.SessionID != "session-legacy" || res.DurationMS != 210 {
		t.Fatalf("text/session/terminal metadata lost: %+v", res)
	}
	if res.Usage == nil || res.Usage.InputTokens != 12 || res.Usage.OutputTokens != 5 ||
		res.Usage.CacheReadInputTokens != 3 || res.Usage.CacheCreationInputTokens != 2 {
		t.Fatalf("signed standalone usage must feed the usage readback: %+v", res.Usage)
	}
	if res.SemanticEvents == 0 || res.ModelEvents == 0 || res.ToolEvents != 3 || res.TerminalEvents != 1 {
		t.Fatalf("semantic/model/tool/terminal observations are incomplete: %+v", res)
	}
}

func TestGrokBuild105ObservedPrefixDoesNotInventTerminal(t *testing.T) {
	res := parseGrokBuildJSONL(grok105ObservedPrefix)
	if res == nil || !res.IsError || res.Subtype != "grok_build_stream_incomplete" ||
		!res.ObservationComplete || res.TerminalEvents != 0 {
		t.Fatalf("proved nonterminal 1.0.5 prefix must stay completely observed but incomplete: %+v", res)
	}
	if res.SemanticEvents == 0 || res.ModelEvents == 0 || res.ToolEvents != 3 {
		t.Fatalf("interrupted semantic/model/tool work must still block a second writer: %+v", res)
	}
}

func TestGrokBuild105StandaloneUsageRequiresProvedEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage string
	}{
		{"missing signature", `{"type":"usage","usage":{"input_tokens":1}}`},
		{"wrong signature type", `{"signature":{},"type":"usage","usage":{"input_tokens":1}}`},
		{"null usage", `{"signature":"s","type":"usage","usage":null}`},
		{"extra top-level field", `{"extra":true,"signature":"s","type":"usage","usage":{"input_tokens":1}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parseGrokBuildJSONL(tc.usage + "\n" + grokLegacyCleanEnd)
			if res == nil || res.ObservationComplete {
				t.Fatalf("unproved standalone usage lookalike must fail closed: %+v", res)
			}
		})
	}
}

func TestGrokBuild105UnknownToolLookalikeFailsClosed(t *testing.T) {
	res := parseGrokBuildJSONL(`{"type":"tool_call_future","content":["opaque"]}` + "\n" + grokLegacyMinimalEnd)
	if res == nil || res.ObservationComplete || res.ToolEvents == 0 {
		t.Fatalf("unknown tool-like content must be counted conservatively and fail closed: %+v", res)
	}
}

func TestGrokBuild105ObservedToolSchemasRejectLookalikes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
	}{
		{"tool call missing content", `{"kind":"tool","locations":[],"rawInput":{},"status":"pending","title":"read","toolCallId":"call-1","toolName":"read_file","type":"tool_call"}`},
		{"tool call wrong raw input type", `{"content":[],"kind":"tool","locations":[],"rawInput":null,"status":"pending","title":"read","toolCallId":"call-1","toolName":"read_file","type":"tool_call"}`},
		{"update missing raw output", `{"content":[],"locations":[],"status":null,"toolCallId":"call-1","type":"tool_call_update"}`},
		{"update mismatched null status", `{"content":[],"locations":[],"rawOutput":{},"status":null,"toolCallId":"call-1","type":"tool_call_update"}`},
		{"update extra data", `{"content":[],"data":"hidden","locations":[],"rawOutput":null,"status":null,"toolCallId":"call-1","type":"tool_call_update"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parseGrokBuildJSONL(tc.event + "\n" + grokLegacyMinimalEnd)
			if res == nil || res.ObservationComplete || res.ToolEvents == 0 {
				t.Fatalf("tool schema lookalike must count as possible work and fail closed: %+v", res)
			}
		})
	}
}

func TestGrokBuild105ContentBearingMetadataFailsClosed(t *testing.T) {
	res := parseGrokBuildJSONL(`{"commands":[],"data":"hidden work","tools":[],"type":"available_commands"}` + "\n" + grokLegacyMinimalEnd)
	if res == nil || res.ObservationComplete || res.SemanticEvents == 0 || res.ModelEvents == 0 {
		t.Fatalf("unparsed content on metadata must block both observation and zero-work proof: %+v", res)
	}
}

func TestGrokBuild105TextSchemaLookalikeFailsClosedButCountsWork(t *testing.T) {
	res := parseGrokBuildJSONL(`{"content":"hidden","data":"visible","type":"text"}` + "\n" + grokLegacyMinimalEnd)
	if res == nil || res.ObservationComplete || res.SemanticEvents == 0 || res.ModelEvents == 0 {
		t.Fatalf("content-bearing text lookalike must remain counted but untrusted: %+v", res)
	}
}

func TestGrokBuildTerminalMustBeSingleFinalEndTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"missing stop reason", `{"type":"text","data":"OK"}` + "\n" + `{"type":"end","sessionId":"s"}`},
		{"empty stop reason", `{"type":"text","data":"OK"}` + "\n" + `{"type":"end","stopReason":"","sessionId":"s"}`},
		{"nonclean stop reason", `{"type":"text","data":"OK"}` + "\n" + `{"type":"end","stopReason":"max_tokens","sessionId":"s"}`},
		{"duplicate terminal", `{"type":"text","data":"OK"}` + "\n" + grokLegacyCleanEnd + "\n" + grokLegacyCleanEnd},
		{"semantic event after terminal", `{"type":"text","data":"OK"}` + "\n" + grokLegacyCleanEnd + "\n" + `{"type":"text","data":"late"}`},
		{"usage event after terminal", `{"type":"text","data":"OK"}` + "\n" + grokLegacyCleanEnd + "\n" + `{"signature":"s","type":"usage","usage":{"input_tokens":1}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parseGrokBuildJSONL(tc.raw)
			if res == nil || !res.IsError || res.Subtype != "grok_build_invalid_terminal" || res.ObservationComplete {
				t.Fatalf("unclean terminal sequence must fail closed: %+v", res)
			}
		})
	}
}

func TestGrokBuildMalformedAndUnknownEventsStayIncomplete(t *testing.T) {
	for _, raw := range []string{
		`{"type":"text","data":"truncated"`,
		`{"type":"future_event","data":"opaque"}` + "\n" + grokLegacyCleanEnd,
	} {
		res := parseGrokBuildJSONL(raw)
		if res == nil || res.ObservationComplete {
			t.Fatalf("malformed or unknown event must fail closed: raw=%q res=%+v", raw, res)
		}
	}
}

func TestGrokBuildLegacyEmbeddedUsageStaysAuthoritative(t *testing.T) {
	raw := `{"type":"text","data":"A"}` + "\n" +
		`{"signature":"s","type":"usage","usage":{"input_tokens":1,"output_tokens":1}}` + "\n" +
		`{"type":"end","stopReason":"end_turn","sessionId":"s","usage":{"input_tokens":9,"output_tokens":7}}`
	res := parseGrokBuildJSONL(raw)
	if res == nil || res.IsError || !res.ObservationComplete {
		t.Fatalf("signed standalone usage plus legacy terminal usage must remain complete: %+v", res)
	}
	if res.Usage == nil || res.Usage.InputTokens != 9 || res.Usage.OutputTokens != 7 {
		t.Fatalf("legacy terminal usage must remain authoritative: %+v", res.Usage)
	}
}

func TestInvokeGrokBuildAcceptsProved105ShapesAndLegacyTerminal(t *testing.T) {
	payload := grok105ObservedPrefix + "\n" + grokLegacyCleanEnd
	bin, _, _ := fakeGrokBuild(t, payload, "", 0)
	cfg := grokBuildTestConfig(t, bin)
	task := &Task{ID: "grok-105-invoke", Type: typeSequence, Dir: t.TempDir(), PreferRunner: grokBuildRunnerName}
	res, _, err := invokeGrokBuild(context.Background(), t.TempDir(), cfg, task, "harmless prompt")
	if err != nil || res == nil || res.IsError || !res.ObservationComplete {
		t.Fatalf("proved 1.0.5 shapes plus legacy terminal must invoke cleanly: res=%+v err=%v", res, err)
	}
	if res.SessionID != "session-legacy" || !strings.Contains(res.Result, "GROK_OK") {
		t.Fatalf("invoke lost terminal identity or semantic text: %+v", res)
	}
}
