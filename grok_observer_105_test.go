package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
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
const grok105PublicUsage = `{"type":"usage","messageId":"message-public-private","stopReason":"tool_use","usage":{"input_tokens":21,"output_tokens":8},"signature":"signature-public-private"}`
const grok105PublicEnd = `{"type":"end","stopReason":"end_turn","sessionId":"session-public-private","requestId":"request-public-private","usage":{"input_tokens":34,"output_tokens":13},"num_turns":3,"modelUsage":{"grok-4.6":{"input_tokens":34,"output_tokens":13}}}`
const grok105PublicEndWithTicks = `{"type":"end","stopReason":"end_turn","sessionId":"session-public-private","requestId":"request-public-private","usage":{"input_tokens":34,"output_tokens":13},"num_turns":3,"modelUsage":{"grok-4.6":{"input_tokens":34,"output_tokens":13}},"total_cost_usd_ticks":731.125}`
const grok105ClosedMetadata = `{"type":"metadata"}`
const grok105ClosedSystemVersion = `{"type":"system.version","version":"1.0.5"}`
const grokOpaqueEndEventID = "event-opaque-private"
const grokOpaqueEndTraceID = "trace-opaque-private"
const grokOpaqueEndChannel = "acct-opaque-channel"

func grok105EndWithAdditiveMetadata(end string) string {
	return strings.TrimSuffix(end, "}") + `,"eventId":"` + grokOpaqueEndEventID + `","traceId":"` + grokOpaqueEndTraceID + `","accounting_channel":"` + grokOpaqueEndChannel + `","closed":true}`
}

func TestGrokBuild105PublicUsageAndEndCompleteWithoutIdentifierExposure(t *testing.T) {
	raw := `{"type":"text","data":"PUBLIC_OK"}` + "\n" + grok105PublicUsage + "\n" + grok105PublicEnd
	res := parseGrokBuildJSONL(raw)
	if res == nil || res.IsError || !res.ObservationComplete {
		t.Fatalf("documented 1.0.5 usage and end envelopes must parse: %+v", res)
	}
	if res.Result != "PUBLIC_OK" || res.SessionID != "" || res.NumTurns != 3 {
		t.Fatalf("public terminal metadata must parse without retaining its session identifier: %+v", res)
	}
	if res.Usage == nil || res.Usage.InputTokens != 34 || res.Usage.OutputTokens != 13 {
		t.Fatalf("public end usage must remain authoritative: %+v", res.Usage)
	}
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateValue := range []string{
		"message-public-private",
		"signature-public-private",
		"session-public-private",
		"request-public-private",
		"grok-4.6",
	} {
		if strings.Contains(string(encoded), privateValue) {
			t.Fatalf("public identifiers and opaque metadata must not be retained or exposed: %s", encoded)
		}
	}
}

func TestGrokBuild105PublicEndTicksAreShapeOnly(t *testing.T) {
	rawPrefix := `{"type":"text","data":"PUBLIC_OK"}` + "\n" + grok105PublicUsage + "\n"
	want := parseGrokBuildJSONL(rawPrefix + grok105PublicEnd)
	got := parseGrokBuildJSONL(rawPrefix + grok105PublicEndWithTicks)
	if got == nil || got.IsError || !got.ObservationComplete {
		t.Fatalf("documented 1.0.5 public end with numeric total_cost_usd_ticks must parse: %+v", got)
	}
	if got.TotalCostUSD != 0 {
		t.Fatalf("total_cost_usd_ticks must not contribute to TotalCostUSD: %+v", got)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("total_cost_usd_ticks must not be retained or alter parser/accounting output: got=%+v want=%+v", got, want)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "total_cost_usd_ticks") || strings.Contains(string(encoded), "731.125") {
		t.Fatalf("total_cost_usd_ticks name/value must not be exposed: %s", encoded)
	}
}

func TestGrokBuild105PublicEndTicksRejectLegacyAndWrongTypes(t *testing.T) {
	legacyWithTicks := strings.TrimSuffix(grokLegacyCleanEnd, "}") + `,"total_cost_usd_ticks":731.125}`
	if res := parseGrokBuildJSONL(legacyWithTicks); res == nil || !res.IsError || res.ObservationComplete {
		t.Fatalf("legacy end containing total_cost_usd_ticks must fail closed: %+v", res)
	}

	for _, tc := range []struct {
		name  string
		value string
	}{
		{"string", `"731.125"`},
		{"object", `{}`},
		{"boolean", `true`},
		{"null", `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			end := strings.Replace(grok105PublicEndWithTicks, "731.125", tc.value, 1)
			res := parseGrokBuildJSONL(end)
			if res == nil || !res.IsError || res.ObservationComplete {
				t.Fatalf("non-number total_cost_usd_ticks must fail closed: %+v", res)
			}
		})
	}
}

func TestGrokBuild105PublicEndTicksRejectExtraOrMissingPublicCoordinate(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  string
	}{
		{
			"content-bearing extra field",
			strings.TrimSuffix(grok105PublicEndWithTicks, "}") + `,"data":"hidden"}`,
		},
		{
			"missing requestId",
			strings.Replace(grok105PublicEndWithTicks, `,"requestId":"request-public-private"`, "", 1),
		},
		{
			"missing modelUsage",
			strings.Replace(grok105PublicEndWithTicks, `,"modelUsage":{"grok-4.6":{"input_tokens":34,"output_tokens":13}}`, "", 1),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parseGrokBuildJSONL(tc.end)
			if res == nil || !res.IsError || res.ObservationComplete {
				t.Fatalf("ticks-bearing public end outside the closed coordinate must fail closed: %+v", res)
			}
		})
	}
}

func TestGrokBuild105PublicEndTicksMustBeSingleAndFinal(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail string
	}{
		{"duplicate end", grok105PublicEndWithTicks},
		{"text tail", `{"type":"text","data":"late"}`},
		{"error tail", `{"type":"error","message":"late"}`},
		{"unknown tail", `{"type":"future_event"}`},
		{"malformed tail", `not-json`},
		{"invalid usage tail", `{"type":"usage","messageId":"m","stopReason":"end_turn","usage":{},"signature":"s","extra":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parseGrokBuildJSONL(grok105PublicEndWithTicks + "\n" + tc.tail)
			if res == nil || !res.IsError || res.ObservationComplete {
				t.Fatalf("every nonempty record after a ticks-bearing end must fail closed: %+v", res)
			}
		})
	}
}

func TestGrokBuild105PublicEndRetainsLegacyOptionalSpendFields(t *testing.T) {
	publicEndWithSpend := strings.TrimSuffix(grok105PublicEnd, "}") + `,"total_cost_usd":0.25,"duration_ms":610}`
	res := parseGrokBuildJSONL(`{"type":"text","data":"OK"}` + "\n" + publicEndWithSpend)
	if res == nil || res.IsError || !res.ObservationComplete || res.SessionID != "" {
		t.Fatalf("documented end with proven optional spend fields must parse privately: %+v", res)
	}
	if res.TotalCostUSD != 0.25 || res.DurationMS != 610 {
		t.Fatalf("legacy optional spend fields were not retained: %+v", res)
	}
}

func TestGrokBuild105PublicUsageEnvelopeRejectsWrongOrMissingFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage string
	}{
		{"missing messageId", `{"type":"usage","stopReason":"end_turn","usage":{},"signature":"s"}`},
		{"missing stopReason", `{"type":"usage","messageId":"m","usage":{},"signature":"s"}`},
		{"wrong messageId type", `{"type":"usage","messageId":7,"stopReason":"end_turn","usage":{},"signature":"s"}`},
		{"wrong stopReason type", `{"type":"usage","messageId":"m","stopReason":{},"usage":{},"signature":"s"}`},
		{"wrong usage type", `{"type":"usage","messageId":"m","stopReason":"end_turn","usage":[],"signature":"s"}`},
		{"wrong signature type", `{"type":"usage","messageId":"m","stopReason":"end_turn","usage":{},"signature":null}`},
		{"extra top-level field", `{"type":"usage","messageId":"m","stopReason":"end_turn","usage":{},"signature":"s","extra":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parseGrokBuildJSONL(tc.usage + "\n" + grokLegacyMinimalEnd)
			if res == nil || !res.IsError || res.ObservationComplete {
				t.Fatalf("malformed public usage envelope must fail closed: %+v", res)
			}
		})
	}
}

func TestGrokBuild105PublicEndEnvelopeRejectsWrongExtraOrMissingCoreFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  string
	}{
		{"missing requestId", `{"type":"end","stopReason":"end_turn","sessionId":"s","usage":{},"num_turns":1,"modelUsage":{}}`},
		{"missing modelUsage", `{"type":"end","stopReason":"end_turn","sessionId":"s","requestId":"r","usage":{},"num_turns":1}`},
		{"missing stopReason", `{"type":"end","sessionId":"s","requestId":"r","usage":{},"num_turns":1,"modelUsage":{}}`},
		{"wrong requestId type", `{"type":"end","stopReason":"end_turn","sessionId":"s","requestId":7,"usage":{},"num_turns":1,"modelUsage":{}}`},
		{"wrong modelUsage type", `{"type":"end","stopReason":"end_turn","sessionId":"s","requestId":"r","usage":{},"num_turns":1,"modelUsage":[]}`},
		{"wrong sessionId type", `{"type":"end","stopReason":"end_turn","sessionId":7,"requestId":"r","usage":{},"num_turns":1,"modelUsage":{}}`},
		{"wrong usage type", `{"type":"end","stopReason":"end_turn","sessionId":"s","requestId":"r","usage":[],"num_turns":1,"modelUsage":{}}`},
		{"content-bearing extra field", `{"type":"end","stopReason":"end_turn","sessionId":"s","requestId":"r","usage":{},"num_turns":1,"modelUsage":{},"data":"hidden"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parseGrokBuildJSONL(`{"type":"text","data":"work"}` + "\n" + tc.end)
			if res == nil || !res.IsError || res.ObservationComplete {
				t.Fatalf("malformed public end envelope must fail closed: %+v", res)
			}
			if res.SessionID != "" {
				t.Fatalf("invalid public end must not retain its session identifier: %+v", res)
			}
		})
	}
}

func TestGrokBuild105PublicEndMustBeSingleFinalEndTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"non-end_turn", strings.Replace(grok105PublicEnd, `"stopReason":"end_turn"`, `"stopReason":"max_tokens"`, 1)},
		{"duplicate end", grok105PublicEnd + "\n" + grok105PublicEnd},
		{"nonempty semantic tail", grok105PublicEnd + "\n" + `{"type":"text","data":"late"}`},
		{"malformed after end", grok105PublicEnd + "\n" + `not-json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parseGrokBuildJSONL(tc.raw)
			if res == nil || !res.IsError || res.Subtype != "grok_build_invalid_terminal" || res.ObservationComplete {
				t.Fatalf("public end must be one final end_turn event: %+v", res)
			}
			if res.SessionID != "" {
				t.Fatalf("public terminal identifiers must not be retained on failure: %+v", res)
			}
		})
	}
}

func TestGrokBuild105PublicFieldsDoNotLoosenLegacyEnvelopes(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"legacy usage plus messageId only", `{"type":"usage","messageId":"m","usage":{},"signature":"s"}` + "\n" + grokLegacyMinimalEnd},
		{"legacy end plus requestId only", `{"type":"end","stopReason":"end_turn","sessionId":"s","requestId":"r"}`},
		{"legacy end plus modelUsage only", `{"type":"end","stopReason":"end_turn","sessionId":"s","modelUsage":{}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parseGrokBuildJSONL(tc.raw)
			if res == nil || !res.IsError || res.ObservationComplete {
				t.Fatalf("hybrid public/legacy envelope must fail closed: %+v", res)
			}
		})
	}
}

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
	root := admitDirectInvoke(t, "", task)
	res, _, err := invokeGrokBuild(context.Background(), root, cfg, task, "harmless prompt")
	if err != nil || res == nil || res.IsError || !res.ObservationComplete {
		t.Fatalf("proved 1.0.5 shapes plus legacy terminal must invoke cleanly: res=%+v err=%v", res, err)
	}
	if res.SessionID != "session-legacy" || !strings.Contains(res.Result, "GROK_OK") {
		t.Fatalf("invoke lost terminal identity or semantic text: %+v", res)
	}
}

func grok105AssertNoOpaqueExposure(t *testing.T, res *claudeResult, values ...string) {
	t.Helper()
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		if strings.Contains(string(encoded), value) {
			t.Fatalf("opaque identifiers and additive metadata values must not be retained or exposed: %s", encoded)
		}
	}
}

func TestGrokBuild105AdditiveEndMetadataIsAcceptedWithoutRetention(t *testing.T) {
	rawPrefix := `{"type":"text","data":"PUBLIC_OK"}` + "\n" + grok105PublicUsage + "\n"
	for _, tc := range []struct {
		name string
		base string
	}{
		{"public end", grok105PublicEnd},
		{"public end with ticks", grok105PublicEndWithTicks},
		{"legacy end", grokLegacyCleanEnd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := parseGrokBuildJSONL(rawPrefix + tc.base)
			got := parseGrokBuildJSONL(rawPrefix + grok105EndWithAdditiveMetadata(tc.base))
			if got == nil || got.IsError || !got.ObservationComplete || got.TerminalEvents != 1 {
				t.Fatalf("additive top-level end metadata must parse: %+v", got)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("additive end metadata must not be retained or alter parser/accounting output: got=%+v want=%+v", got, want)
			}
			grok105AssertNoOpaqueExposure(t, got,
				grokOpaqueEndEventID, grokOpaqueEndTraceID, grokOpaqueEndChannel,
				"eventId", "traceId", "accounting_channel")
		})
	}
}

func TestGrokBuild105UsageAndMetadataTailsAfterEndAreAcceptedWithoutRetention(t *testing.T) {
	publicPrefix := `{"type":"text","data":"PUBLIC_OK"}` + "\n"
	legacyPrefix := `{"type":"text","data":"OK"}` + "\n"
	legacyUsage := `{"signature":"signature-legacy-private","type":"usage","usage":{"input_tokens":1,"output_tokens":1}}`

	t.Run("public usage and metadata tails", func(t *testing.T) {
		want := parseGrokBuildJSONL(publicPrefix + grok105PublicEnd)
		got := parseGrokBuildJSONL(publicPrefix + grok105PublicEnd + "\n" + grok105PublicUsage + "\n" + grok105ClosedMetadata)
		if got == nil || got.IsError || !got.ObservationComplete || got.TerminalEvents != 1 {
			t.Fatalf("known-valid usage and closed metadata tails after public end must parse: %+v", got)
		}
		if got.Result != "PUBLIC_OK" || got.SessionID != "" {
			t.Fatalf("post-end tails must not change semantic text or retain public session identity: %+v", got)
		}
		if got.Usage == nil || got.Usage.InputTokens != 34 || got.Usage.OutputTokens != 13 {
			t.Fatalf("trailing usage must not displace the accepted end usage: %+v", got.Usage)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("closed post-end accounting/metadata must not alter parser/accounting output: got=%+v want=%+v", got, want)
		}
		grok105AssertNoOpaqueExposure(t, got,
			"message-public-private", "signature-public-private",
			"session-public-private", "request-public-private")
	})

	t.Run("legacy usage and system.version tails", func(t *testing.T) {
		want := parseGrokBuildJSONL(legacyPrefix + grokLegacyCleanEnd)
		got := parseGrokBuildJSONL(legacyPrefix + grokLegacyCleanEnd + "\n" + legacyUsage + "\n" + grok105ClosedSystemVersion)
		if got == nil || got.IsError || !got.ObservationComplete || got.TerminalEvents != 1 {
			t.Fatalf("known-valid usage and closed metadata tails after legacy end must parse: %+v", got)
		}
		if got.Result != "OK" || got.SessionID != "session-legacy" {
			t.Fatalf("legacy post-end tails must keep semantic text and resumable session identity: %+v", got)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("legacy post-end accounting/metadata must not alter parser/accounting output: got=%+v want=%+v", got, want)
		}
		grok105AssertNoOpaqueExposure(t, got, "signature-legacy-private")
	})

	t.Run("additive end plus usage and metadata tails", func(t *testing.T) {
		want := parseGrokBuildJSONL(publicPrefix + grok105PublicEndWithTicks)
		got := parseGrokBuildJSONL(publicPrefix + grok105EndWithAdditiveMetadata(grok105PublicEndWithTicks) + "\n" + grok105PublicUsage + "\n" + grok105ClosedMetadata)
		if got == nil || got.IsError || !got.ObservationComplete || got.TerminalEvents != 1 {
			t.Fatalf("additive end plus closed post-end tails must parse: %+v", got)
		}
		if got.TotalCostUSD != 0 {
			t.Fatalf("total_cost_usd_ticks must not contribute to TotalCostUSD: %+v", got)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("additive end plus tails must not retain metadata or alter accounting: got=%+v want=%+v", got, want)
		}
		grok105AssertNoOpaqueExposure(t, got,
			grokOpaqueEndEventID, grokOpaqueEndTraceID, grokOpaqueEndChannel,
			"731.125", "total_cost_usd_ticks",
			"message-public-private", "signature-public-private")
	})
}

func TestGrokBuild105RejectsForbiddenTailsAfterEnd(t *testing.T) {
	prefix := `{"type":"text","data":"OK"}` + "\n" + grok105PublicEnd + "\n"
	for _, tc := range []struct {
		name string
		tail string
	}{
		{"text", `{"type":"text","data":"late"}`},
		{"thought", `{"type":"thought","data":"late"}`},
		{"reasoning", `{"type":"reasoning","data":"late"}`},
		{"model", `{"type":"model"}`},
		{"tool", `{"type":"tool"}`},
		{"tool_call", `{"content":[],"kind":"tool","locations":[],"rawInput":{},"status":"pending","title":"read","toolCallId":"call-late","toolName":"read_file","type":"tool_call"}`},
		{"tool_call_update", `{"content":[],"locations":[],"rawOutput":null,"status":null,"toolCallId":"call-late","type":"tool_call_update"}`},
		{"error", `{"type":"error","message":"late"}`},
		{"content-bearing metadata", `{"type":"metadata","data":"hidden"}`},
		{"malformed json", `not-json`},
		{"unknown event", `{"type":"future_event"}`},
		{"invalid usage", `{"type":"usage","messageId":"m","stopReason":"end_turn","usage":{},"signature":"s","extra":true}`},
		{"invalid metadata", `{"type":"metadata","extra":true}`},
		{"duplicate end", grok105PublicEnd},
		{"non-end_turn", `{"type":"end","stopReason":"max_tokens","sessionId":"s"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parseGrokBuildJSONL(prefix + tc.tail)
			if res == nil || !res.IsError || res.Subtype != "grok_build_invalid_terminal" || res.ObservationComplete {
				t.Fatalf("forbidden tail after end must fail closed: %+v", res)
			}
			if res.SessionID != "" {
				t.Fatalf("failed public terminal must not retain its session identifier: %+v", res)
			}
			grok105AssertNoOpaqueExposure(t, res, "call-late", "hidden")
		})
	}
}

func TestGrokBuild105EndRejectsContentSemanticToolOrErrorShapedFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  string
	}{
		{"data", strings.TrimSuffix(grok105PublicEnd, "}") + `,"data":"hidden"}`},
		{"message", strings.TrimSuffix(grokLegacyMinimalEnd, "}") + `,"message":"late"}`},
		{"content", strings.TrimSuffix(grok105PublicEnd, "}") + `,"content":[]}`},
		{"error", strings.TrimSuffix(grokLegacyCleanEnd, "}") + `,"error":"late"}`},
		{"toolName", strings.TrimSuffix(grok105PublicEnd, "}") + `,"toolName":"read_file"}`},
		{"toolCallId", strings.TrimSuffix(grokLegacyMinimalEnd, "}") + `,"toolCallId":"call-end"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := parseGrokBuildJSONL(`{"type":"text","data":"OK"}` + "\n" + tc.end)
			if res == nil || !res.IsError || res.ObservationComplete {
				t.Fatalf("content/semantic/tool/error-shaped end fields must fail closed: %+v", res)
			}
			grok105AssertNoOpaqueExposure(t, res, "hidden", "late", "read_file", "call-end")
		})
	}
}

const grok105CompletePublicStdout = `{"type":"text","data":"PUBLIC_OK"}` + "\n" + grok105PublicEnd
const grok105CompleteLegacyStdout = grok105ObservedPrefix + "\n" + grokLegacyCleanEnd
const grok105AncillaryWarningToken = "ANCILLARY_GROK_WARN_TOKEN_DO_NOT_RETAIN"

// Representative ANSI/plain Grok CLI warning/ancillary reporting lines. Values are fixtures only
// and must never appear in parsed results.
var grok105RepresentativeAncillaryWarnings = []string{
	"\x1b[90m15:55:17.906\x1b[0m \x1b[33mwarn\x1b[0m: disabled background tool reporter token=" + grok105AncillaryWarningToken + "-01",
	"\x1b[90m15:55:17.907\x1b[0m \x1b[33mwarn\x1b[0m: disabled background tool reporter token=" + grok105AncillaryWarningToken + "-02",
	"\x1b[90m15:55:17.908\x1b[0m \x1b[33mwarn\x1b[0m: failed background connection reporting token=" + grok105AncillaryWarningToken + "-03",
	"\x1b[90m15:55:17.909\x1b[0m \x1b[33mwarn\x1b[0m: reporter error payload { code: timeout, retry: false } token=" + grok105AncillaryWarningToken + "-04",
	"\x1b[90m15:55:17.910\x1b[0m \x1b[33mwarn\x1b[0m: error opening ancillary log stream (syscall error EPIPE) token=" + grok105AncillaryWarningToken + "-05",
	"\x1b[33mwarn\x1b[0m: background connection reporting failed: connection refused token=" + grok105AncillaryWarningToken + "-06",
	"\x1b[1m\x1b[33mwarn\x1b[0m: background connection reporting failed: connection reset by peer token=" + grok105AncillaryWarningToken + "-07",
	"\x1b[90m15:55:17.913\x1b[0m \x1b[33mwarn\x1b[0m: error opening ancillary log stream (syscall error EPIPE) token=" + grok105AncillaryWarningToken + "-08",
	"\x1b[33mwarn\x1b[0m: background connection reporting failed: connection refused token=" + grok105AncillaryWarningToken + "-09",
	"\x1b[33mwarn\x1b[0m: background session reporting failed: connection reset by peer token=" + grok105AncillaryWarningToken + "-10",
	"\x1b[33mwarn\x1b[0m: session reporting failed, dropping ancillary error stream: transport error: error sending request token=" + grok105AncillaryWarningToken + "-11",
	"\x1b[33mwarn\x1b[0m: session reporting failed after successful model turn token=" + grok105AncillaryWarningToken + "-12",
}

func grok105AncillaryStderr(n int) string {
	return strings.Join(grok105RepresentativeAncillaryWarnings[:n], "\n")
}

func grok105InvokeWithIO(t *testing.T, stdout, stderr string, exitCode int) (*claudeResult, error) {
	t.Helper()
	bin, _, _ := fakeGrokBuild(t, stdout, stderr, exitCode)
	cfg := grokBuildTestConfig(t, bin)
	task := &Task{ID: "grok-105-io", Type: typeSequence, Dir: t.TempDir(), PreferRunner: grokBuildRunnerName}
	root := admitDirectInvoke(t, "", task)
	res, _, err := invokeGrokBuild(context.Background(), root, cfg, task, "harmless prompt")
	return res, err
}

func grok105AssertFailedClosed(t *testing.T, res *claudeResult, err error) {
	t.Helper()
	if err == nil || res == nil || !res.IsError {
		t.Fatalf("must fail closed: res=%+v err=%v", res, err)
	}
}

func TestGrokBuild105CompleteStdoutOmitsAncillaryWarningStderr(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
		n      int
		want   string
	}{
		{"11 public", grok105CompletePublicStdout, 11, "PUBLIC_OK"},
		{"12 legacy", grok105CompleteLegacyStdout, 12, "GROK_OK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stderr := grok105AncillaryStderr(tc.n)
			if strings.Count(stderr, "\n") != tc.n-1 {
				t.Fatalf("fixture must contain %d warning lines", tc.n)
			}
			want := parseGrokBuildJSONL(tc.stdout)
			got, err := grok105InvokeWithIO(t, tc.stdout, stderr, 0)
			if err != nil || got == nil || got.IsError || !got.ObservationComplete {
				t.Fatalf("complete stdout plus %d ancillary warnings must succeed: res=%+v err=%v", tc.n, got, err)
			}
			if got.Result != tc.want || got.TerminalEvents != 1 {
				t.Fatalf("stdout semantic/terminal result lost: %+v", got)
			}
			// Invocation adds process/channel diagnostics unavailable to the standalone parser.
			// Compare every pre-existing business and policy field independently of that projection.
			gotBusiness, wantBusiness := *got, *want
			gotBusiness.GrokDiagnostics, wantBusiness.GrokDiagnostics = nil, nil
			if !reflect.DeepEqual(gotBusiness, wantBusiness) {
				t.Fatalf("ancillary stderr must not alter the stdout-alone parse: got=%+v want=%+v", got, want)
			}
			grok105AssertNoOpaqueExposure(t, got, grok105AncillaryWarningToken,
				"reporting failed", "session reporting", "background connection",
				"EPIPE", "syscall error", "\x1b[")
		})
	}
}

func TestGrokBuild105CompleteStdoutStructuredStderrFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stderr string
	}{
		{"text", `{"type":"text","data":"hidden-stderr-text"}`},
		{"thought", `{"type":"thought","data":"hidden-stderr-thought"}`},
		{"tool", `{"type":"tool"}`},
		{"tool_call", `{"content":[],"kind":"tool","locations":[],"rawInput":{},"status":"pending","title":"read","toolCallId":"call-stderr","toolName":"read_file","type":"tool_call"}`},
		{"tool_call_update", `{"content":[],"locations":[],"rawOutput":null,"status":null,"toolCallId":"call-stderr","type":"tool_call_update"}`},
		{"error", `{"type":"error","message":"hidden-stderr-error"}`},
		{"unknown", `{"type":"future_event"}`},
		{"duplicate-end", grok105PublicEnd},
		{"malformed-structured", `{"type":"text","data":"truncated"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := grok105InvokeWithIO(t, grok105CompletePublicStdout, tc.stderr, 0)
			grok105AssertFailedClosed(t, res, err)
			if res.ObservationComplete {
				t.Fatalf("structured stderr must make the combined observation incomplete: %+v", res)
			}
			if res.Result == "PUBLIC_OK" && !res.IsError {
				t.Fatalf("structured stderr must not keep a successful semantic result: %+v", res)
			}
			grok105AssertNoOpaqueExposure(t, res, "hidden-stderr-text", "hidden-stderr-thought",
				"hidden-stderr-error", "call-stderr")
		})
	}
}

func TestGrokBuild105IncompleteOrInvalidStdoutKeepsAncillaryStderrFailed(t *testing.T) {
	stderr := grok105AncillaryStderr(12)
	for _, tc := range []struct {
		name   string
		stdout string
	}{
		{"incomplete prefix", grok105ObservedPrefix},
		{"missing terminal", `{"type":"text","data":"PUBLIC_OK"}`},
		{"duplicate terminal", grok105CompletePublicStdout + "\n" + grok105PublicEnd},
		{"semantic after end", grok105CompletePublicStdout + "\n" + `{"type":"text","data":"late"}`},
		{"tool after end", grok105CompletePublicStdout + "\n" + `{"type":"tool"}`},
		{"error after end", grok105CompletePublicStdout + "\n" + `{"type":"error","message":"late"}`},
		{"malformed after end", grok105CompletePublicStdout + "\n" + `not-json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := grok105InvokeWithIO(t, tc.stdout, stderr, 0)
			grok105AssertFailedClosed(t, res, err)
			grok105AssertNoOpaqueExposure(t, res, grok105AncillaryWarningToken)
			if res.Result == "PUBLIC_OK" && res.ObservationComplete && !res.IsError {
				t.Fatalf("invalid/incomplete stdout must not become success via ancillary stderr: %+v", res)
			}
		})
	}
}

func TestGrokBuild105NonzeroExitPreservesRootCauseWithAncillaryStderr(t *testing.T) {
	t.Run("complete stdout plus warnings", func(t *testing.T) {
		res, err := grok105InvokeWithIO(t, grok105CompletePublicStdout, grok105AncillaryStderr(11), 1)
		grok105AssertFailedClosed(t, res, err)
		if !res.IsError || (res.ObservationComplete && res.Result == "PUBLIC_OK" && res.Subtype == "") {
			t.Fatalf("nonzero exit must not become a clean semantic success: %+v", res)
		}
		grok105AssertNoOpaqueExposure(t, res, grok105AncillaryWarningToken)
	})
	t.Run("exact oidc diagnostic", func(t *testing.T) {
		res, err := grok105InvokeWithIO(t, `{"type":"system.version","version":"1.0.5"}`, grokOIDCNoAuthContextDiagnostic, 1)
		grok105AssertFailedClosed(t, res, err)
		if res.Subtype != "grok_build_process_auth_exact" {
			t.Fatalf("exact OIDC stderr must keep auth root cause: %+v err=%v", res, err)
		}
		if res.Result != grokOIDCNoAuthContextDiagnostic {
			t.Fatalf("exact diagnostic must remain the classified result: %+v", res)
		}
	})
}

const grokDiagnosticCanary = "xai-DIAGNOSTIC-PRIVATE-CONTENT-DO-NOT-RETAIN"

// The task/event readback below runs the real Grok invocation and runner with an offline process.
// A valid business Result may contain text; only the NEW diagnostic projection must be value-free.
func TestGrokDiagnosticsTaskEventReadback(t *testing.T) {
	text := `{"type":"text","data":"` + grokDiagnosticCanary + `"}`
	end := grok105PublicEnd
	valid := text + "\n" + end
	for _, tc := range []struct {
		name, stdout, stderr, defect, source, stop string
		ends, legacyEnds                           int
		complete, success                          bool
	}{
		{"valid public", valid, "", "none", "none", "end_turn", 1, 1, true, true},
		{"valid usage metadata tail", valid + "\n" + grok105PublicUsage + "\n" + grok105ClosedMetadata, "", "none", "none", "end_turn", 1, 1, true, true},
		{"ancillary stderr", valid, "warning " + grokDiagnosticCanary, "none", "none", "end_turn", 1, 1, true, true},
		{"closed stderr metadata", valid, grok105ClosedMetadata, "none", "none", "end_turn", 1, 1, true, true},
		{"missing end", text, "", "missing_end", "observation", "missing", 0, 0, true, false},
		{"duplicate end", valid + "\n" + end, "", "duplicate_end", "stdout", "end_turn", 2, 1, true, false},
		{"third end counted", valid + "\n" + end + "\n" + end, "", "duplicate_end", "stdout", "end_turn", 3, 1, true, false},
		{"bad end shape", text + "\n" + `{"type":"end","stopReason":"end_turn","message":"` + grokDiagnosticCanary + `"}`, "", "invalid_end_shape", "stdout", "end_turn", 1, 1, true, false},
		{"abnormal end", text + "\n" + strings.Replace(end, "end_turn", "max_tokens", 1), "", "abnormal_stop_reason", "stdout", "max_tokens", 1, 1, true, false},
		{"private stop reason", text + "\n" + strings.Replace(end, "end_turn", grokDiagnosticCanary, 1), "", "abnormal_stop_reason", "stdout", "unknown", 1, 1, true, false},
		{"bad end decode", text + "\n" + `{"type":"end","stopReason":123}`, "", "event_decode_before_end", "stdout", "invalid_type", 1, 0, true, false},
		{"bad json before end", text + "\n" + grokDiagnosticCanary + "\n" + end, "", "malformed_json_before_end", "stdout", "end_turn", 1, 1, true, false},
		{"bad json after end", valid + "\n" + grokDiagnosticCanary, "", "malformed_json_after_end", "stdout", "end_turn", 1, 1, true, false},
		{"missing type before end", text + "\n" + `{"private":"` + grokDiagnosticCanary + `"}` + "\n" + end, "", "missing_type_before_end", "stdout", "end_turn", 1, 1, true, false},
		{"missing type after end", valid + "\n" + `{"private":"` + grokDiagnosticCanary + `"}`, "", "missing_type_after_end", "stdout", "end_turn", 1, 1, true, false},
		{"unknown before end", text + "\n" + `{"type":"` + grokDiagnosticCanary + `","rawInput":{"secret":"` + grokDiagnosticCanary + `"}}` + "\n" + end, "", "unknown_event_before_end", "stdout", "end_turn", 1, 1, true, false},
		{"unknown after end", valid + "\n" + `{"type":"` + grokDiagnosticCanary + `"}`, "", "disallowed_event_after_end", "stdout", "end_turn", 1, 1, true, false},
		{"invalid shape before end", text + "\n" + `{"type":"metadata","data":"` + grokDiagnosticCanary + `"}` + "\n" + end, "", "invalid_event_shape_before_end", "stdout", "end_turn", 1, 1, true, false},
		{"invalid shape after end", valid + "\n" + `{"type":"usage","usage":"` + grokDiagnosticCanary + `"}`, "", "event_decode_after_end", "stdout", "end_turn", 1, 1, true, false},
		{"stderr semantic merge", valid, text, "disallowed_event_after_end", "stderr", "end_turn", 1, 1, true, false},
		{"stderr duplicate merge", valid, end, "duplicate_end", "stderr", "end_turn", 2, 1, true, false},
		{"stderr malformed merge", valid, `{"private":"` + grokDiagnosticCanary, "malformed_json_after_end", "stderr", "end_turn", 1, 1, true, false},
		{"CRLF source boundary", strings.ReplaceAll(valid, "\n", "\r\n") + "\r\n", text, "disallowed_event_after_end", "stderr", "end_turn", 1, 1, true, false},
		{"stdout scanner loss", text + "\n" + strings.Repeat("x", 4*1024*1024) + "\n" + end, "", "scanner_loss", "stdout", "missing", 0, 0, false, false},
		{"stderr parser scanner loss", valid, `{"type":"` + strings.Repeat("x", 4*1024*1024) + `"}`, "scanner_loss", "stderr", "end_turn", 1, 1, false, false},
		{"stderr adapter scanner loss", valid, strings.Repeat("x", 8*1024*1024), "stderr_scanner_loss", "stderr", "end_turn", 1, 1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			bin, calls := fakeGrokBuildCountedStderrFile(t, tc.stdout, tc.stderr, 0)
			cfg := grokBuildTestConfig(t, bin)
			cfg.OwnerRoutingEnforced = false
			task := newTask(root, cfg, typeSequence, "offline diagnostic fixture", t.TempDir(), []string{"fixture"}, 1)
			task.PreferRunner, task.RunnerExplicit, task.Model = grokBuildRunnerName, true, "sonnet"
			if err := saveTask(root, task); err != nil {
				t.Fatal(err)
			}
			// Exercise the production parser/adapter directly as well as through the real runner.
			parsed := parseGrokBuildJSONLChannels(grokBuildJSONObservation(tc.stdout, tc.stderr, nil), len(tc.stdout))
			if parsed.TerminalEvents != tc.legacyEnds || parsed.GrokDiagnostics.ObservedEndCount != tc.ends {
				t.Fatalf("legacy and diagnostic counts must stay separate: legacy=%d diagnostic=%d", parsed.TerminalEvents, parsed.GrokDiagnostics.ObservedEndCount)
			}
			if parsed.IsError == tc.success {
				t.Fatalf("parser decision changed: success=%v result=%+v", tc.success, parsed)
			}
			if tc.success && parsed.Result != grokDiagnosticCanary {
				t.Fatal("diagnostic redaction must not change the existing business Result")
			}
			if err := runTaskVia(context.Background(), root, cfg, task, grokBuildRunnerName); err != nil {
				t.Fatal(err)
			}
			got, err := loadTask(root, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus, wantEvent := statusHeld, evHeld
			if tc.success {
				wantStatus, wantEvent = statusDone, evStepOK
			}
			if got.Status != wantStatus || got.Attempts != 0 || got.PreferRunner != grokBuildRunnerName ||
				got.NotBeforeEpoch != 0 || got.ResumeAtEpoch != 0 || countProductCalls(t, calls) != 1 {
				t.Fatalf("decision/retry/fallback changed: %+v", got)
			}
			if got.LastRouteAttempt == nil || got.LastRouteAttempt.GrokDiagnostics == nil {
				t.Fatal("missing task diagnostic readback")
			}
			d := got.LastRouteAttempt.GrokDiagnostics
			if d.TerminalDefect != tc.defect || d.DefectSource != tc.source || d.StopReason != tc.stop ||
				d.ObservedEndCount != tc.ends || d.ParserScanComplete != tc.complete || d.Subtype != grokBuildDiagnosticSubtype(parsed.Subtype) {
				t.Fatalf("diagnostic mismatch: %+v", d)
			}
			if d.ObservationOrder != "stdout_then_stderr" || d.StdioEOF != "unknown" ||
				d.ExitCode == nil || *d.ExitCode != 0 || d.Signal == nil || *d.Signal != 0 ||
				d.TimedOut == nil || *d.TimedOut || d.ProcessError != "none" || d.WaitError != "none" {
				t.Fatalf("zero/false must be observed and EOF unknown: %+v", d)
			}
			events, truncated, err := readEvents(eventsPath(root, task.ID))
			if err != nil || truncated {
				t.Fatalf("event readback err=%v truncated=%v", err, truncated)
			}
			found := false
			for _, event := range events {
				if event.Type == evRetry || event.Actor == "runner:policy-fallback" {
					t.Fatalf("unexpected retry/fallback event: %+v", event)
				}
				if event.Type != wantEvent {
					continue
				}
				raw, err := json.Marshal(event.Detail["grok_diagnostics"])
				if err != nil {
					t.Fatal(err)
				}
				var eventDiagnostics grokBuildDiagnostics
				if err := json.Unmarshal(raw, &eventDiagnostics); err != nil || !reflect.DeepEqual(*d, eventDiagnostics) {
					t.Fatalf("event lost task diagnostic projection: %s err=%v", raw, err)
				}
				grokAssertSafeDiagnostics(t, raw)
				found = true
			}
			if !found {
				t.Fatal("missing result-bearing event diagnostic readback")
			}
			raw, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			grokAssertSafeDiagnostics(t, raw)
		})
	}
}

func grokAssertSafeDiagnostics(t *testing.T, raw []byte) {
	t.Helper()
	for _, secret := range []string{grokDiagnosticCanary, "message-public-private", "session-public-private",
		"request-public-private", "signature-public-private", "rawInput", "secret", "grok-4.6"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("new diagnostics retained private material %q: %s", secret, raw)
		}
	}
}

func TestGrokDiagnosticsParserAndProcessErrorReadback(t *testing.T) {
	for _, tc := range []struct {
		name, category string
		exit           int
		timeout        bool
	}{
		{"nonzero exit", "exit", 7, false},
		{"signal", "signal", -1, false},
		{"step timeout", "signal", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := testRoot(t)
			payload := `{"type":"text","data":"` + grokDiagnosticCanary + `"}` + "\n" + grok105PublicEnd + "\n" + grok105PublicEnd
			bin, calls := fakeGrokBuildCounted(t, payload, "", tc.exit)
			if tc.timeout {
				script, err := os.ReadFile(bin)
				if err != nil {
					t.Fatal(err)
				}
				// The production one-minute step deadline kills this exact fake process group.
				// exec avoids leaving a shell descendant if the test is interrupted.
				script = []byte(strings.TrimSuffix(string(script), "exit 0\n") + "exec sleep 70\n")
				if err := os.WriteFile(bin, script, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cfg := grokBuildTestConfig(t, bin)
			cfg.OwnerRoutingEnforced = true
			task := ownerBackendGrokTask(t, root, cfg, t.TempDir())
			task.Attempts = 2
			if err := saveTask(root, task); err != nil {
				t.Fatal(err)
			}
			if err := runTaskVia(context.Background(), root, cfg, task, grokBuildRunnerName); err != nil {
				t.Fatal(err)
			}
			assertGrokTerminalUnknownHeld(t, root, task, calls, 2, fallbackInvalidTerminal, 3, 3, 0, false, grokDiagnosticCanary)
			got, err := loadTask(root, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			d := got.LastRouteAttempt.GrokDiagnostics
			if d == nil || d.Subtype != "grok_build_invalid_terminal" || d.TerminalDefect != "duplicate_end" ||
				d.ObservedEndCount != 2 || d.ProcessError != tc.category || d.WaitError != tc.category ||
				d.TimedOut == nil || *d.TimedOut != tc.timeout || d.StdioEOF != "unknown" {
				t.Fatalf("parser error must coexist with original process outcome: %+v", d)
			}
			if tc.category == "exit" {
				if d.ExitCode == nil || *d.ExitCode != 7 || d.Signal == nil || *d.Signal != 0 {
					t.Fatalf("exit status missing: %+v", d)
				}
			} else if d.ExitCode != nil || d.Signal == nil || *d.Signal != 9 {
				t.Fatalf("signal must not be reported as a known numeric exit code: %+v", d)
			}
			events, _, err := readEvents(eventsPath(root, task.ID))
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, event := range events {
				if event.Type == evHeld {
					raw, _ := json.Marshal(event.Detail["grok_diagnostics"])
					var eventDiagnostics grokBuildDiagnostics
					if err := json.Unmarshal(raw, &eventDiagnostics); err != nil || !reflect.DeepEqual(*d, eventDiagnostics) {
						t.Fatalf("held event lost coexisting errors: %s err=%v", raw, err)
					}
					grokAssertSafeDiagnostics(t, raw)
					found = true
				}
			}
			if !found {
				t.Fatal("missing held event")
			}
		})
	}
}

func TestGrokDiagnosticsUnknownProcessFactsAndClosedValues(t *testing.T) {
	res := parseGrokBuildJSONL(`{"type":"end","stopReason":"` + grokDiagnosticCanary + `"}`)
	d := res.GrokDiagnostics
	if d.ExitCode != nil || d.Signal != nil || d.TimedOut != nil || d.WaitError != "unknown" || d.ProcessError != "unknown" ||
		d.DefectSource != "unknown" || d.StdioEOF != "unknown" || d.StopReason != "unknown" {
		t.Fatalf("standalone parser cannot assert process facts: %+v", d)
	}
	raw, _ := json.Marshal(d)
	for _, unknown := range []string{`"exit_code":null`, `"signal":null`, `"timed_out":null`} {
		if !strings.Contains(string(raw), unknown) {
			t.Fatalf("unknown must remain explicit in JSON: %s", raw)
		}
	}
	for _, tc := range []struct {
		err            error
		category, wait string
	}{
		{errors.New(grokDiagnosticCanary), "other", "unknown"},
		{context.DeadlineExceeded, "deadline", "unknown"},
		{context.Canceled, "canceled", "unknown"},
		{exec.ErrWaitDelay, "wait_delay", "wait_delay"},
	} {
		d.process(&exec.Cmd{}, tc.err, false)
		if d.ProcessError != tc.category || d.WaitError != tc.wait || d.ExitCode != nil || d.Signal != nil {
			t.Fatalf("unproved Wait/exit facts must remain unknown: %+v", d)
		}
		raw, _ := json.Marshal(d)
		grokAssertSafeDiagnostics(t, raw)
	}
	if grokBuildDiagnosticSubtype(grokDiagnosticCanary) != "unknown" {
		t.Fatal("unknown subtype must be closed")
	}
	for _, value := range []string{`null`, `[]`, `{}`, `1`, `true`} {
		if got := grokBuildDiagnosticStopReason(json.RawMessage(value)); got != "invalid_type" {
			t.Fatalf("stop reason field type was not controlled: %q", got)
		}
	}
}
