package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// Issue #37 (onlyjco): image_url content parts were silently dropped while
// flattening content — image-only requests died with the misleading
// "no user message in request", mixed content returned 200 while the model
// answered "I don't see any image". v0.8.66 turned both into an explicit
// rejection. The 2026-10-20 CN live matrix (see image_input.go) showed the
// drop was an Intl-realm artifact: the very passthrough that "silently lost"
// the parts delivers pixels on the CN gateway. Contract now:
//
//	image_forward: true  (default) → image parts ride along, no error
//	image_forward: false          → v0.8.66 honest rejection
//
// These tests pin both halves.

// imageOnlyMessages reproduces Repro A: a single user message carrying only
// an image_url part.
func imageOnlyMessages(t *testing.T) []openAIMessage {
	t.Helper()
	raw := `[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]`
	return unmarshalMessages(t, raw)
}

// mixedMessages reproduces Repro B: text + image in the latest user turn.
func mixedMessages(t *testing.T) []openAIMessage {
	t.Helper()
	raw := `[{"role":"user","content":[{"type":"text","text":"describe this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,BBBB"}}]}]`
	return unmarshalMessages(t, raw)
}

func unmarshalMessages(t *testing.T, raw string) []openAIMessage {
	t.Helper()
	var msgs []openAIMessage
	if err := json.Unmarshal([]byte(raw), &msgs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return msgs
}

// TestBuildQoderBodyForwardsImageOnly: an image-only turn must reach the body
// builder (it used to fail with "no user message in request"), and the data
// URI must survive into the forwarded messages.
func TestBuildQoderBodyForwardsImageOnly(t *testing.T) {
	setImageForward(true)
	defer setImageForward(true)
	raw, err := buildQoderBody(&openAIRequest{Model: "qfmodel", Messages: imageOnlyMessages(t)}, "qfmodel", "default")
	if err != nil {
		t.Fatalf("image-only request must be forwarded: %v", err)
	}
	if !strings.Contains(string(raw), "data:image/png;base64,AAAA") {
		t.Fatal("image part must ride along in the messages payload")
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if biz, ok := body["business"].(map[string]any); ok {
		if name, _ := biz["name"].(string); name == "" {
			t.Fatal("image-only turn needs a non-empty business.name")
		}
	}
}

// TestBuildQoderBodyForwardsMixed: text+image keeps both the prompt text and
// the image part.
func TestBuildQoderBodyForwardsMixed(t *testing.T) {
	setImageForward(true)
	defer setImageForward(true)
	raw, err := buildQoderBody(&openAIRequest{Model: "qfmodel", Messages: mixedMessages(t)}, "qfmodel", "default")
	if err != nil {
		t.Fatalf("mixed content must be forwarded: %v", err)
	}
	s := string(raw)
	if !strings.Contains(s, "describe this") || !strings.Contains(s, "data:image/png;base64,BBBB") {
		t.Fatalf("mixed content lost a part: %s", s[:min(len(s), 400)])
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	cc, _ := body["chat_context"].(map[string]any)
	txt, _ := cc["text"].(map[string]any)
	if got, _ := txt["text"].(string); got != "describe this" {
		t.Fatalf("chat_context.text must stay the text part, got %q", got)
	}
}

// TestBuildQoderBodyRejectsImagesWhenForwardDisabled pins the opt-out.
func TestBuildQoderBodyRejectsImagesWhenForwardDisabled(t *testing.T) {
	setImageForward(false)
	defer setImageForward(true)
	for name, msgs := range map[string][]openAIMessage{
		"image-only": imageOnlyMessages(t),
		"mixed":      mixedMessages(t),
	} {
		_, err := buildQoderBody(&openAIRequest{Model: "qfmodel", Messages: msgs}, "qfmodel", "default")
		if err == nil {
			t.Fatalf("%s: image request must fail when image_forward=false", name)
		}
		for _, marker := range []string{"image_url", "text-only"} {
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(marker)) {
				t.Fatalf("%s: error copy must name the %q limitation: %v", name, marker, err)
			}
		}
	}
}

// TestImageForwardDefaultsOn: a bare reconfigure (no image_forward key) must
// restore the shipped default rather than keeping a previously configured off.
func TestImageForwardDefaultsOn(t *testing.T) {
	setImageForward(false)
	// pluginapi.ConfigYAML is a []byte field, so the lifecycle payload carries
	// it base64-encoded — mirror that here.
	bare := base64.StdEncoding.EncodeToString([]byte("checkin_auto: false"))
	configure([]byte(`{"config_yaml":"` + bare + `"}`))
	if !imageForwardEnabled() {
		t.Fatal("bare reconfigure must reset image_forward to the default (true)")
	}
	off := base64.StdEncoding.EncodeToString([]byte("image_forward: false"))
	configure([]byte(`{"config_yaml":"` + off + `"}`))
	if imageForwardEnabled() {
		t.Fatal("explicit image_forward: false must be honored")
	}
	setImageForward(true)
}

func TestBuildQoderBodyPlainAndStructuredTextStillWork(t *testing.T) {
	// Plain-string content (regression).
	_, err := buildQoderBody(&openAIRequest{
		Model:    "qfmodel",
		Messages: unmarshalMessages(t, `[{"role":"system","content":"sys"},{"role":"user","content":"hello"}]`),
	}, "qfmodel", "default")
	if err != nil {
		t.Fatalf("plain text must keep working: %v", err)
	}
	// Structured content with ONLY text parts keeps working.
	_, err = buildQoderBody(&openAIRequest{
		Model:    "qfmodel",
		Messages: unmarshalMessages(t, `[{"role":"system","content":"sys"},{"role":"user","content":[{"type":"text","text":"hi there"}]}]`),
	}, "qfmodel", "default")
	if err != nil {
		t.Fatalf("text-only structured content must keep working: %v", err)
	}
	// A request with neither text nor images still fails honestly.
	_, err = buildQoderBody(&openAIRequest{
		Model:    "qfmodel",
		Messages: unmarshalMessages(t, `[{"role":"system","content":"sys"}]`),
	}, "qfmodel", "default")
	if err == nil || !strings.Contains(err.Error(), "no user message") {
		t.Fatalf("text-less, image-less request must fail: %v", err)
	}
}

func TestBuildQoderBodyHistoryImagesNotRetroactive(t *testing.T) {
	// An old screenshot in the HISTORY must not fail every follow-up turn.
	setImageForward(false)
	defer setImageForward(true)
	msgs := unmarshalMessages(t, `[`+
		`{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,OLD"}}]},`+
		`{"role":"assistant","content":"cannot see images"},`+
		`{"role":"user","content":"and now just text"}`+
		`]`)
	_, err := buildQoderBody(&openAIRequest{Model: "qfmodel", Messages: msgs}, "qfmodel", "default")
	if err != nil {
		t.Fatalf("history image must not fail a text follow-up: %v", err)
	}
}

func TestLatestUserImagePartCounters(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    int
	}{
		{"image only", `[{"type":"image_url","image_url":{"url":"x"}}]`, 1},
		{"two images", `[{"type":"image_url","image_url":{"url":"x"}},{"type":"image_url","image_url":{"url":"y"}}]`, 2},
		{"mixed", `[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"x"}}]`, 1},
		{"text only", `[{"type":"text","text":"a"}]`, 0},
		{"plain string", `"hello"`, 0},
		{"null", `null`, 0},
		{"not an array", `{"type":"image_url"}`, 0},
		{"unparseable", `[{"type":`, 0},
		{"other part type", `[{"type":"input_audio","input_audio":{"data":"x"}}]`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := imagePartsInContent(tc.content); got != tc.want {
				t.Fatalf("imagePartsInContent(%s) = %d, want %d", tc.content, got, tc.want)
			}
		})
	}
	// Latest-user scoping: an assistant turn with images after the user turn
	// is not the trigger.
	msgs := unmarshalMessages(t, `[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"x"}}]}]`)
	if got := latestUserImageParts(msgs); got != 0 {
		t.Fatalf("assistant-turn images must not count: %d", got)
	}
}

func TestExtractLatestUserPromptUnchanged(t *testing.T) {
	// The image gate must not change text extraction itself.
	msgs := mixedMessages(t)
	if got := extractLatestUserPrompt(msgs); got != "describe this" {
		t.Fatalf("prompt extraction regressed: %q", got)
	}
}
