package chat

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

var generousMediaLimits = MediaLimits{MaxParts: 8, MaxDecodedBytes: 1 << 20}

func inlineDataURI(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func quotedJSON(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

func TestContentUnionPreservesMissingNullStringAndParts(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"assistant","tool_calls":[{"id":"missing"}]},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"null"}]},` +
		`{"role":"user","content":"hello"},` +
		`{"role":"user","content":[{"type":"text","text":"world"}]}` +
		`]}`)
	in, aerr := DecodeInbound(body)
	if aerr != nil {
		t.Fatalf("decode: %v", aerr)
	}
	wantKinds := []ContentKind{ContentKindMissing, ContentKindNull, ContentKindString, ContentKindParts}
	for i, want := range wantKinds {
		if got := in.Messages[i].Content.Kind(); got != want {
			t.Fatalf("message %d kind=%v, want %v", i, got, want)
		}
	}
	if _, aerr := in.ValidateAndClassify(generousMediaLimits); aerr != nil {
		t.Fatalf("valid tool/text history rejected: %v", aerr)
	}
	raw, err := json.Marshal(Sanitize(in, ptrInt64(64)))
	if err != nil {
		t.Fatalf("marshal canonical request: %v", err)
	}
	s := string(raw)
	if strings.Count(s, `"content":null`) != 1 {
		t.Fatalf("null must survive while missing remains absent: %s", s)
	}
	if strings.Contains(s, `"model"`) {
		t.Fatalf("canonical request must not contain provider model: %s", s)
	}
}

func TestContentUnionRejectsOtherJSONShapes(t *testing.T) {
	for _, content := range []string{`true`, `42`, `{}`, `{"text":"x"}`} {
		body := []byte(`{"messages":[{"role":"user","content":` + content + `}]}`)
		if _, aerr := DecodeInbound(body); aerr == nil || aerr.Status != 400 {
			t.Fatalf("content=%s: expected 400, got %v", content, aerr)
		}
	}
}

func TestClassifyUsesCompleteHistory(t *testing.T) {
	png := inlineDataURI("image/png", []byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'})
	body := []byte(`{"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"first"},{"type":"image_url","image_url":{"url":` + quotedJSON(png) + `}}]},` +
		`{"role":"assistant","content":"ack"},` +
		`{"role":"user","content":"last message is plain text"}` +
		`]}`)
	in, aerr := DecodeInbound(body)
	if aerr != nil {
		t.Fatalf("decode: %v", aerr)
	}
	got, aerr := in.ValidateAndClassify(generousMediaLimits)
	if aerr != nil {
		t.Fatalf("classify: %v", aerr)
	}
	if got != ModalityMultimodal {
		t.Fatalf("media in earlier history must route multimodal, got %v", got)
	}
}

func TestClassifyStringsAndTextPartsAsText(t *testing.T) {
	body := []byte(`{"messages":[` +
		`{"role":"system","content":"be concise"},` +
		`{"role":"user","content":[{"type":"text","text":"hello"},{"type":"text","text":" world"}]}` +
		`]}`)
	in, aerr := DecodeInbound(body)
	if aerr != nil {
		t.Fatalf("decode: %v", aerr)
	}
	got, aerr := in.ValidateAndClassify(generousMediaLimits)
	if aerr != nil || got != ModalityText {
		t.Fatalf("text-only request: modality=%v err=%v", got, aerr)
	}
	raw, err := json.Marshal(Sanitize(in, ptrInt64(64)))
	if err != nil {
		t.Fatalf("marshal sanitized text request: %v", err)
	}
	if !strings.Contains(string(raw), `"content":"hello world"`) || strings.Contains(string(raw), `"type":"text"`) {
		t.Fatalf("text-only parts must canonicalize to a provider-neutral string: %s", raw)
	}
}

func TestValidateImagesMIMEBase64AndMagic(t *testing.T) {
	valid := map[string][]byte{
		"image/jpeg": {0xff, 0xd8, 0xff, 0xdb},
		"image/png":  {'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'},
		"image/webp": []byte("RIFF1234WEBP"),
	}
	for mime, data := range valid {
		t.Run(mime, func(t *testing.T) {
			uri := inlineDataURI(mime, data)
			body := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":` + quotedJSON(uri) + `}}]}]}`)
			in, aerr := DecodeInbound(body)
			if aerr != nil {
				t.Fatalf("decode: %v", aerr)
			}
			got, aerr := in.ValidateAndClassify(generousMediaLimits)
			if aerr != nil || got != ModalityMultimodal {
				t.Fatalf("valid %s: modality=%v err=%v", mime, got, aerr)
			}
		})
	}

	badURLs := []string{
		"https://example.com/image.png",
		inlineDataURI("application/pdf", []byte("%PDF")),
		inlineDataURI("image/png", []byte{0xff, 0xd8, 0xff}),
		"data:image/png;base64,not!base64",
	}
	for _, url := range badURLs {
		body := []byte(`{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":` + quotedJSON(url) + `}}]}]}`)
		in, decodeErr := DecodeInbound(body)
		if decodeErr != nil {
			t.Fatalf("validation case should structurally decode: %v", decodeErr)
		}
		if _, aerr := in.ValidateAndClassify(generousMediaLimits); aerr == nil || aerr.Status != 400 {
			t.Fatalf("url=%q: expected explicit 400, got %v", url, aerr)
		}
	}
}

func TestAudioPartsAreValidatedAndClassifiedSeparately(t *testing.T) {
	wav := base64.StdEncoding.EncodeToString([]byte("RIFF\x04\x00\x00\x00WAVEfmt "))
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"` + wav + `","format":"wav"}}]}]}`)
	in, err := DecodeInbound(body)
	if err != nil {
		t.Fatalf("audio must pass the structural decoder, got %v", err)
	}
	if modality, err := in.ValidateAndClassify(generousMediaLimits); err != nil || modality != ModalityAudio {
		t.Fatalf("audio modality=%v err=%v, want audio/nil", modality, err)
	}

	bad := []byte(`{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"` + wav + `","format":"mp3"}}]}]}`)
	in, err = DecodeInbound(bad)
	if err != nil {
		t.Fatalf("mismatched audio should structurally decode, got %v", err)
	}
	if _, err := in.ValidateAndClassify(generousMediaLimits); err == nil || err.Status != 400 {
		t.Fatalf("mismatched audio must fail validation, got %v", err)
	}
}

func TestMediaOnlyAllowedInUserMessages(t *testing.T) {
	png := inlineDataURI("image/png", []byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'})
	for _, role := range []string{"system", "assistant", "tool"} {
		body := []byte(`{"messages":[{"role":` + quotedJSON(role) + `,"content":[{"type":"image_url","image_url":{"url":` + quotedJSON(png) + `}}]}]}`)
		in, aerr := DecodeInbound(body)
		if aerr != nil {
			t.Fatalf("decode role %s: %v", role, aerr)
		}
		if _, aerr := in.ValidateAndClassify(generousMediaLimits); aerr == nil || aerr.Status != 400 {
			t.Fatalf("role=%s: expected 400, got %v", role, aerr)
		}
	}
}

func TestMessageRoleIsClosedAndToolCallIDIsRequired(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"content":"missing role"}]}`,
		`{"messages":[{"role":"developer","content":"unsupported intersection"}]}`,
		`{"messages":[{"role":"future","content":"unsupported"}]}`,
		`{"messages":[{"role":"tool","content":"result"}]}`,
	} {
		in, decodeErr := DecodeInbound([]byte(body))
		if decodeErr != nil {
			t.Fatalf("validation case should structurally decode: %v", decodeErr)
		}
		if _, aerr := in.ValidateAndClassify(generousMediaLimits); aerr == nil || aerr.Status != 400 {
			t.Fatalf("body=%s: expected role/tool contract 400, got %v", body, aerr)
		}
	}

	in, aerr := DecodeInbound([]byte(`{"messages":[{"role":"tool","tool_call_id":"call_1","content":"result"}]}`))
	if aerr != nil {
		t.Fatalf("decode valid tool message: %v", aerr)
	}
	if modality, aerr := in.ValidateAndClassify(generousMediaLimits); aerr != nil || modality != ModalityText {
		t.Fatalf("valid tool message rejected: modality=%v err=%v", modality, aerr)
	}
}

func TestVideoAndUnknownFileParts(t *testing.T) {
	validMP4 := base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 20, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'})
	valid := []byte(`{"messages":[{"role":"user","content":[{"type":"video_url","video_url":{"url":"data:video/mp4;base64,` + validMP4 + `"}}]}]}`)
	in, err := DecodeInbound(valid)
	if err != nil {
		t.Fatalf("decode valid video: %v", err)
	}
	if modality, err := in.ValidateAndClassify(generousMediaLimits); err != nil || modality != ModalityMultimodal {
		t.Fatalf("valid video: modality=%v err=%v", modality, err)
	}
	for _, part := range []string{
		`{"type":"video_url","video_url":{"url":"data:video/mp4;base64,AAAA"}}`,
		`{"type":"file","file":{"file_data":"data:application/pdf;base64,JVBERg=="}}`,
		`{"type":"future_media","data":"AAAA"}`,
	} {
		body := []byte(`{"messages":[{"role":"user","content":[` + part + `]}]}`)
		in, aerr := DecodeInbound(body)
		if aerr != nil {
			continue // unknown variants are rejected by the structural decoder.
		}
		if _, aerr := in.ValidateAndClassify(generousMediaLimits); aerr == nil || aerr.Status != 400 {
			t.Fatalf("part=%s: expected 400, got %v", part, aerr)
		}
	}
}

func TestMediaLimitsAreCumulativeAndInjected(t *testing.T) {
	pngData := []byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'}
	uri := inlineDataURI("image/png", pngData)
	body := []byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":` + quotedJSON(uri) + `}},` +
		`{"type":"image_url","image_url":{"url":` + quotedJSON(uri) + `}}` +
		`]}]}`)
	in, aerr := DecodeInbound(body)
	if aerr != nil {
		t.Fatalf("decode: %v", aerr)
	}
	if _, aerr := in.ValidateAndClassify(MediaLimits{MaxParts: 1, MaxDecodedBytes: 100}); aerr == nil {
		t.Fatal("max-parts limit must reject")
	}
	if _, aerr := in.ValidateAndClassify(MediaLimits{MaxParts: 2, MaxDecodedBytes: int64(len(pngData)*2 - 1)}); aerr == nil {
		t.Fatal("cumulative decoded-byte limit must reject")
	}
	if got, aerr := in.ValidateAndClassify(MediaLimits{MaxParts: 2, MaxDecodedBytes: int64(len(pngData) * 2)}); aerr != nil || got != ModalityMultimodal {
		t.Fatalf("exact limits should accept: modality=%v err=%v", got, aerr)
	}
}

func TestPromptEstimateChargesMediaBase64(t *testing.T) {
	png := inlineDataURI("image/png", append([]byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'}, make([]byte, 512)...))
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":` + quotedJSON(png) + `}}]}]}`)
	in, aerr := DecodeInbound(body)
	if aerr != nil {
		t.Fatalf("decode: %v", aerr)
	}
	mediaEstimate := in.PromptEstimate()
	textEstimate := EstimatePromptTokens([]Message{{Role: "user", Content: StringContent("look")}})
	if mediaEstimate <= textEstimate+int64(len(png))/2 {
		t.Fatalf("media base64 must be conservatively charged: text=%d media=%d encoded=%d", textEstimate, mediaEstimate, len(png))
	}
	textContextEstimate := in.TextPromptEstimate()
	if textContextEstimate >= mediaEstimate/2 {
		t.Fatalf("Kimi text-context estimate must not treat transport base64 as text tokens: full=%d text=%d", mediaEstimate, textContextEstimate)
	}
}

func TestCompletionRequestAttachesModelOnlyAtProviderBoundary(t *testing.T) {
	in, aerr := DecodeInbound([]byte(`{"model":"client-choice","messages":[{"role":"user","content":"hi"}]}`))
	if aerr != nil {
		t.Fatalf("decode: %v", aerr)
	}
	canonical, err := json.Marshal(Sanitize(in, ptrInt64(64)))
	if err != nil {
		t.Fatalf("marshal canonical: %v", err)
	}
	if strings.Contains(string(canonical), `"model"`) {
		t.Fatalf("client/provider model leaked into canonical request: %s", canonical)
	}
	provider, err := json.Marshal(Sanitize(in, ptrInt64(64)).WithModel("provider-model"))
	if err != nil {
		t.Fatalf("marshal provider request: %v", err)
	}
	if !strings.Contains(string(provider), `"model":"provider-model"`) || strings.Contains(string(provider), "client-choice") {
		t.Fatalf("provider must own model id: %s", provider)
	}
}

func TestSanitizeWireGolden_TextAndAssistantToolCalls(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "string content stays a string",
			body: `{"messages":[{"role":"user","content":"hello"}]}`,
			want: `{"messages":[{"role":"user","content":"hello"}],"stream":false,"max_tokens":64}`,
		},
		{
			name: "absent assistant tool-call content stays absent",
			body: `{"messages":[{"role":"assistant","tool_calls":[{"id":"c1"}]}]}`,
			want: `{"messages":[{"role":"assistant","tool_calls":[{"id":"c1"}]}],"stream":false,"max_tokens":64}`,
		},
		{
			name: "null assistant tool-call content stays null",
			body: `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c1"}]}]}`,
			want: `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c1"}]}],"stream":false,"max_tokens":64}`,
		},
		{
			name: "legacy empty assistant tool-call content remains omitted",
			body: `{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"c1"}]}]}`,
			want: `{"messages":[{"role":"assistant","tool_calls":[{"id":"c1"}]}],"stream":false,"max_tokens":64}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in, aerr := DecodeInbound([]byte(tc.body))
			if aerr != nil {
				t.Fatalf("decode: %v", aerr)
			}
			got, err := json.Marshal(Sanitize(in, ptrInt64(64)))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("wire drift\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}
