package main

import (
	"context"
	"testing"
)

// grok105CompleteStream is the exact complete semantic stream shape observed from the direct
// Grok CLI 1.0.5 transport (see the post-login transport hold receipt): available_commands,
// thought, text, a standalone usage event, and the terminal end event. Cardex must observe this
// stream as complete; treating the standalone usage event as unknown must not silently degrade
// the observation that the zero-work fallback proof and semantic gates depend on.
const grok105CompleteStream = `{"type":"available_commands","tools":["read_file"]}` + "\n" +
	`{"type":"thought","data":"planning the answer"}` + "\n" +
	`{"type":"text","data":"GROK_"}` + "\n" +
	`{"type":"text","data":"OK"}` + "\n" +
	`{"type":"usage","usage":{"input_tokens":12,"cache_read_input_tokens":3,"cache_creation_input_tokens":2,"output_tokens":5}}` + "\n" +
	`{"type":"end","stopReason":"end_turn","sessionId":"session-105","num_turns":2,"duration_ms":210}`

func TestGrokBuild105StandaloneUsageEventCompletesObservation(t *testing.T) {
	res := parseGrokBuildJSONL(grok105CompleteStream)
	if res == nil || res.IsError || !res.ObservationComplete {
		t.Fatalf("complete 1.0.5 thought/text/usage/end stream must parse cleanly: %+v", res)
	}
	if res.Result != "GROK_OK" || res.SessionID != "session-105" || res.DurationMS != 210 {
		t.Fatalf("text/session/terminal metadata lost: %+v", res)
	}
	if res.Usage == nil || res.Usage.InputTokens != 12 || res.Usage.OutputTokens != 5 ||
		res.Usage.CacheReadInputTokens != 3 || res.Usage.CacheCreationInputTokens != 2 {
		t.Fatalf("standalone usage event must feed the usage readback: %+v", res.Usage)
	}
	if res.SemanticEvents == 0 || res.ModelEvents == 0 {
		t.Fatalf("thought/text plus real usage must count as semantic/model work: %+v", res)
	}
}

func TestGrokBuild105EndEmbeddedUsageStaysAuthoritative(t *testing.T) {
	// Older supported contract: usage embedded in the terminal end event. When both forms appear,
	// stream order decides — the terminal event remains the authoritative final accounting.
	raw := `{"type":"text","data":"A"}` + "\n" +
		`{"type":"usage","usage":{"input_tokens":1,"output_tokens":1}}` + "\n" +
		`{"type":"end","stopReason":"end_turn","sessionId":"s","usage":{"input_tokens":9,"output_tokens":7}}`
	res := parseGrokBuildJSONL(raw)
	if res == nil || res.IsError || !res.ObservationComplete {
		t.Fatalf("mixed usage/end stream must stay a complete observation: %+v", res)
	}
	if res.Usage == nil || res.Usage.InputTokens != 9 || res.Usage.OutputTokens != 7 {
		t.Fatalf("terminal embedded usage must remain authoritative: %+v", res.Usage)
	}
}

func TestGrokBuild105ZeroUsageEventIsNotModelWork(t *testing.T) {
	raw := `{"type":"usage","usage":{"input_tokens":0,"output_tokens":0}}` + "\n" +
		`{"type":"end","stopReason":"end_turn","sessionId":"s"}`
	res := parseGrokBuildJSONL(raw)
	if res == nil || !res.ObservationComplete {
		t.Fatalf("a zero standalone usage event is known accounting metadata, not an unknown event: %+v", res)
	}
	if res.ModelEvents != 0 {
		t.Fatalf("zero-token usage must not fabricate model work: %+v", res)
	}
}

func TestInvokeGrokBuildAccepts105CompleteStream(t *testing.T) {
	bin, _, _ := fakeGrokBuild(t, grok105CompleteStream, "", 0)
	cfg := grokBuildTestConfig(t, bin)
	task := &Task{ID: "grok-105-invoke", Type: typeSequence, Dir: t.TempDir(), PreferRunner: grokBuildRunnerName}
	res, _, err := invokeGrokBuild(context.Background(), t.TempDir(), cfg, task, "harmless prompt")
	if err != nil || res == nil || res.IsError || !res.ObservationComplete {
		t.Fatalf("1.0.5 complete stream must invoke with a complete observation: res=%+v err=%v", res, err)
	}
	if res.SessionID != "session-105" || res.Result != "GROK_OK" {
		t.Fatalf("1.0.5 invoke lost terminal identity: %+v", res)
	}
}
