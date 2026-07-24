package chat

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
)

// ContentKind identifies the only four JSON states accepted for message
// content. Missing and null stay distinct because an assistant tool-call turn
// may legitimately use either, while user/tool text must not silently turn a
// malformed null into an empty string.
type ContentKind uint8

const (
	ContentKindMissing ContentKind = iota
	ContentKindNull
	ContentKindString
	ContentKindParts
)

// Content is the provider-independent, strict Chat Completions content union:
// an absent field, null, a string, or an array of supported ContentParts. Its
// representation is deliberately private so an invalid mixed union cannot be
// constructed accidentally; callers can use the constructors and accessors.
type Content struct {
	kind  ContentKind
	text  string
	parts []ContentPart
}

// MissingContent constructs an absent content field.
func MissingContent() Content { return Content{kind: ContentKindMissing} }

// NullContent constructs a literal JSON null content field.
func NullContent() Content { return Content{kind: ContentKindNull} }

// StringContent constructs a string content field (including an empty string).
func StringContent(text string) Content { return Content{kind: ContentKindString, text: text} }

// PartsContent constructs an array content field. The slice is copied so the
// canonical request cannot change underneath validation/routing.
func PartsContent(parts []ContentPart) Content {
	return Content{kind: ContentKindParts, parts: append([]ContentPart(nil), parts...)}
}

// Kind reports which member of the JSON union this value contains.
func (c Content) Kind() ContentKind { return c.kind }

// StringValue returns the string member and whether content is a string.
func (c Content) StringValue() (string, bool) {
	return c.text, c.kind == ContentKindString
}

// Parts returns a copy of the parts member and whether content is an array.
func (c Content) Parts() ([]ContentPart, bool) {
	if c.kind != ContentKindParts {
		return nil, false
	}
	return append([]ContentPart(nil), c.parts...), true
}

func (c *Content) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		*c = NullContent()
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		*c = StringContent(text)
		return nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		*c = PartsContent(parts)
		return nil
	}
	return errors.New("content must be null, a string, or an array")
}

func (c Content) MarshalJSON() ([]byte, error) {
	switch c.kind {
	case ContentKindMissing:
		// Message.MarshalJSON owns omission. Returning null keeps standalone
		// Content values valid JSON without inventing a fifth wire state.
		return []byte("null"), nil
	case ContentKindNull:
		return []byte("null"), nil
	case ContentKindString:
		return json.Marshal(c.text)
	case ContentKindParts:
		return json.Marshal(c.parts)
	default:
		return nil, errors.New("invalid content kind")
	}
}

const (
	PartTypeText       = "text"
	PartTypeImageURL   = "image_url"
	PartTypeVideoURL   = "video_url"
	PartTypeInputAudio = "input_audio"
)

// ContentPart is one member of array-form content. Exactly one payload is
// populated according to Type; its custom decoder rejects unknown types,
// cross-variant fields, and unknown fields rather than forwarding them.
type ContentPart struct {
	Type       string
	Text       string
	ImageURL   *ImageURL
	VideoURL   *VideoURL
	InputAudio *InputAudio
}

// ImageURL is the OpenAI-compatible image_url payload. Only an inline base64
// data URI is accepted; Detail is a closed optional hint retained canonically.
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// VideoURL is Qwen's OpenAI-compatible video_url payload. The gateway accepts
// only a strict inline MP4 data URI; it never downloads a remote URL.
type VideoURL struct {
	URL string `json:"url"`
}

// InputAudio is the OpenAI-compatible input_audio payload. Data is raw base64
// (not a data URI); Format is restricted to wav or mp3 during validation.
type InputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

func (p *ContentPart) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return errors.New("content part must be an object")
	}
	var typ string
	if rawType, ok := fields["type"]; !ok || json.Unmarshal(rawType, &typ) != nil || typ == "" {
		return errors.New("content part type is required")
	}

	allowed := map[string]bool{"type": true}
	*p = ContentPart{Type: typ}
	switch typ {
	case PartTypeText:
		allowed["text"] = true
		rawText, ok := fields["text"]
		if !ok || json.Unmarshal(rawText, &p.Text) != nil {
			return errors.New("text part requires string text")
		}
	case PartTypeImageURL:
		allowed["image_url"] = true
		rawImage, ok := fields["image_url"]
		if !ok {
			return errors.New("image_url part requires image_url")
		}
		image, err := decodeImageURL(rawImage)
		if err != nil {
			return err
		}
		p.ImageURL = &image
	case PartTypeVideoURL:
		allowed["video_url"] = true
		rawVideo, ok := fields["video_url"]
		if !ok {
			return errors.New("video_url part requires video_url")
		}
		video, err := decodeVideoURL(rawVideo)
		if err != nil {
			return err
		}
		p.VideoURL = &video
	case PartTypeInputAudio:
		allowed["input_audio"] = true
		rawAudio, ok := fields["input_audio"]
		if !ok {
			return errors.New("input_audio part requires input_audio")
		}
		audio, err := decodeInputAudio(rawAudio)
		if err != nil {
			return err
		}
		p.InputAudio = &audio
	default:
		return fmt.Errorf("unsupported content part type %q", typ)
	}
	for name := range fields {
		if !allowed[name] {
			return fmt.Errorf("unknown field %q in %s content part", name, typ)
		}
	}
	return nil
}

func decodeImageURL(raw []byte) (ImageURL, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return ImageURL{}, errors.New("image_url must be an object")
	}
	for name := range fields {
		if name != "url" && name != "detail" {
			return ImageURL{}, fmt.Errorf("unknown image_url field %q", name)
		}
	}
	var image ImageURL
	if value, ok := fields["url"]; !ok || json.Unmarshal(value, &image.URL) != nil || image.URL == "" {
		return ImageURL{}, errors.New("image_url.url must be a string")
	}
	if value, ok := fields["detail"]; ok {
		if json.Unmarshal(value, &image.Detail) != nil {
			return ImageURL{}, errors.New("image_url.detail must be a string")
		}
		if image.Detail != "auto" && image.Detail != "low" && image.Detail != "high" {
			return ImageURL{}, errors.New("image_url.detail must be auto, low, or high")
		}
	}
	return image, nil
}

func decodeVideoURL(raw []byte) (VideoURL, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return VideoURL{}, errors.New("video_url must be an object")
	}
	for name := range fields {
		if name != "url" {
			return VideoURL{}, fmt.Errorf("unknown video_url field %q", name)
		}
	}
	var video VideoURL
	if value, ok := fields["url"]; !ok || json.Unmarshal(value, &video.URL) != nil || video.URL == "" {
		return VideoURL{}, errors.New("video_url.url must be a string")
	}
	return video, nil
}

func decodeInputAudio(raw []byte) (InputAudio, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return InputAudio{}, errors.New("input_audio must be an object")
	}
	for name := range fields {
		if name != "data" && name != "format" {
			return InputAudio{}, fmt.Errorf("unknown input_audio field %q", name)
		}
	}
	var audio InputAudio
	if value, ok := fields["data"]; !ok || json.Unmarshal(value, &audio.Data) != nil || audio.Data == "" {
		return InputAudio{}, errors.New("input_audio.data must be a non-empty string")
	}
	if value, ok := fields["format"]; !ok || json.Unmarshal(value, &audio.Format) != nil || audio.Format == "" {
		return InputAudio{}, errors.New("input_audio.format must be a string")
	}
	return audio, nil
}

func (p ContentPart) MarshalJSON() ([]byte, error) {
	switch p.Type {
	case PartTypeText:
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: p.Type, Text: p.Text})
	case PartTypeImageURL:
		if p.ImageURL == nil {
			return nil, errors.New("image_url part has no image_url payload")
		}
		return json.Marshal(struct {
			Type     string   `json:"type"`
			ImageURL ImageURL `json:"image_url"`
		}{Type: p.Type, ImageURL: *p.ImageURL})
	case PartTypeVideoURL:
		if p.VideoURL == nil {
			return nil, errors.New("video_url part has no video_url payload")
		}
		return json.Marshal(struct {
			Type     string   `json:"type"`
			VideoURL VideoURL `json:"video_url"`
		}{Type: p.Type, VideoURL: *p.VideoURL})
	case PartTypeInputAudio:
		if p.InputAudio == nil {
			return nil, errors.New("input_audio part has no input_audio payload")
		}
		return json.Marshal(struct {
			Type       string     `json:"type"`
			InputAudio InputAudio `json:"input_audio"`
		}{Type: p.Type, InputAudio: *p.InputAudio})
	default:
		return nil, fmt.Errorf("unsupported content part type %q", p.Type)
	}
}

// Message is the provider-independent Chat Completions message. Opaque tool
// fields are preserved verbatim for the multi-turn tool loop.
type Message struct {
	Role             string
	Content          Content
	Name             string
	ToolCalls        json.RawMessage
	ToolCallID       string
	ReasoningContent json.RawMessage
}

func (m *Message) UnmarshalJSON(raw []byte) error {
	var wire struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content"`
		Name             string          `json:"name"`
		ToolCalls        json.RawMessage `json:"tool_calls"`
		ToolCallID       string          `json:"tool_call_id"`
		ReasoningContent json.RawMessage `json:"reasoning_content"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return err
	}
	content := MissingContent()
	if wire.Content != nil {
		if err := json.Unmarshal(wire.Content, &content); err != nil {
			return err
		}
	}
	*m = Message{
		Role:             wire.Role,
		Content:          content,
		Name:             wire.Name,
		ToolCalls:        wire.ToolCalls,
		ToolCallID:       wire.ToolCallID,
		ReasoningContent: wire.ReasoningContent,
	}
	return nil
}

func (m Message) MarshalJSON() ([]byte, error) {
	var content json.RawMessage
	omitEmptyToolCallContent := hasToolCalls(m.ToolCalls) && m.Content.kind == ContentKindString && m.Content.text == ""
	if m.Content.kind != ContentKindMissing && !omitEmptyToolCallContent {
		raw, err := json.Marshal(m.Content)
		if err != nil {
			return nil, err
		}
		content = raw
	}
	return json.Marshal(struct {
		Role             string          `json:"role"`
		Content          json.RawMessage `json:"content,omitempty"`
		Name             string          `json:"name,omitempty"`
		ToolCalls        json.RawMessage `json:"tool_calls,omitempty"`
		ToolCallID       string          `json:"tool_call_id,omitempty"`
		ReasoningContent json.RawMessage `json:"reasoning_content,omitempty"`
	}{
		Role:             m.Role,
		Content:          content,
		Name:             m.Name,
		ToolCalls:        m.ToolCalls,
		ToolCallID:       m.ToolCallID,
		ReasoningContent: m.ReasoningContent,
	})
}

// Kept as a package-local alias so existing same-package tests and any pending
// refactor compile while the public canonical name is Message.
type chatMessage = Message

// Modality is the deterministic route classification for a whole request.
type Modality uint8

const (
	ModalityText Modality = iota
	ModalityMultimodal
	// ModalityAudio is a valid, recognized audio request. The current fixed gateway has no
	// audio upstream yet, so app/chat returns AUDIO_UNAVAILABLE before reservation/open. Keeping it
	// distinct from malformed input makes the public protocol ready for a future audio-capable route.
	ModalityAudio
)

func (m Modality) String() string {
	switch m {
	case ModalityMultimodal:
		return "multimodal"
	case ModalityAudio:
		return "audio"
	default:
		return "text"
	}
}

// MediaLimits caps cumulative media work per request. MaxParts counts image, video and audio
// parts (text parts do not consume it); MaxDecodedBytes is the cumulative
// decoded payload size. Non-positive limits disable media while leaving text
// requests available.
type MediaLimits struct {
	MaxParts        int
	MaxDecodedBytes int64
}

// ValidateAndClassify validates every message in the complete history and
// classifies solely by shape: any accepted image/audio part is multimodal;
// strings and text-only arrays are text. It never fetches URLs.
func (in InboundRequest) ValidateAndClassify(limits MediaLimits) (Modality, *apierr.APIError) {
	modality := ModalityText
	mediaParts := 0
	var decodedBytes int64
	for _, message := range in.Messages {
		switch message.Role {
		case "system", "user", "assistant", "tool":
			// Closed intersection supported by both fixed compatibility routes.
		default:
			return ModalityText, contentError("message role must be system, user, assistant, or tool")
		}
		if message.Role == "tool" && strings.TrimSpace(message.ToolCallID) == "" {
			return ModalityText, contentError("tool message requires tool_call_id")
		}
		switch message.Content.kind {
		case ContentKindMissing, ContentKindNull:
			if message.Role != "assistant" || !hasToolCalls(message.ToolCalls) {
				return ModalityText, contentError("content is required unless this is an assistant tool-call turn")
			}
		case ContentKindString:
			// Text route.
		case ContentKindParts:
			for _, part := range message.Content.parts {
				switch part.Type {
				case PartTypeText:
					// Text route.
				case PartTypeImageURL:
					if message.Role != "user" {
						return ModalityText, contentError("media content is only allowed in user messages")
					}
					mediaParts++
					if limits.MaxParts <= 0 || mediaParts > limits.MaxParts {
						return ModalityText, contentError("too many media parts")
					}
					remaining := limits.MaxDecodedBytes - decodedBytes
					if remaining < 0 {
						remaining = 0
					}
					n, err := validateImage(part.ImageURL, remaining)
					if err != nil {
						return ModalityText, contentError(err.Error())
					}
					decodedBytes += n
					if modality != ModalityAudio {
						modality = ModalityMultimodal
					}
				case PartTypeVideoURL:
					if message.Role != "user" {
						return ModalityText, contentError("media content is only allowed in user messages")
					}
					mediaParts++
					if limits.MaxParts <= 0 || mediaParts > limits.MaxParts {
						return ModalityText, contentError("too many media parts")
					}
					remaining := limits.MaxDecodedBytes - decodedBytes
					if remaining < 0 {
						remaining = 0
					}
					n, err := validateVideo(part.VideoURL, remaining)
					if err != nil {
						return ModalityText, contentError(err.Error())
					}
					decodedBytes += n
					if modality != ModalityAudio {
						modality = ModalityMultimodal
					}
				case PartTypeInputAudio:
					if message.Role != "user" {
						return ModalityText, contentError("media content is only allowed in user messages")
					}
					mediaParts++
					if limits.MaxParts <= 0 || mediaParts > limits.MaxParts {
						return ModalityText, contentError("too many media parts")
					}
					remaining := limits.MaxDecodedBytes - decodedBytes
					if remaining < 0 {
						remaining = 0
					}
					n, err := validateAudio(part.InputAudio, remaining)
					if err != nil {
						return ModalityText, contentError(err.Error())
					}
					decodedBytes += n
					modality = ModalityAudio
				default:
					// Construction outside JSON cannot bypass the closed union.
					return ModalityText, contentError("unsupported content part type")
				}
			}
		default:
			return ModalityText, contentError("invalid content")
		}
	}
	return modality, nil
}

func hasToolCalls(raw json.RawMessage) bool {
	var calls []json.RawMessage
	return json.Unmarshal(raw, &calls) == nil && len(calls) > 0
}

func contentError(message string) *apierr.APIError {
	return apierr.NewError(apierr.ErrBadRequest.Status, apierr.ErrBadRequest.Code, message)
}

func validateImage(image *ImageURL, maxDecoded int64) (int64, error) {
	if image == nil {
		return 0, errors.New("image_url payload is required")
	}
	// ADR 0011: a lease the gateway itself issued, named by its RELATIVE fetch path, is the ONE
	// accepted non-data reference. It contributes ZERO decoded bytes — the media never traverses
	// the chat body, which is the whole point of the one-shot upload. Ownership/expiry are verified
	// in the app layer (this package holds no repository); shape alone is decided here.
	// ADR 0011:网关自己签发、以**相对** fetch path 指称的 lease,是唯一被接受的非 data 引用。它计
	// **零**解码字节——媒体从不经过 chat body,这正是一次性上传的全部目的。归属/时效在 app 层校验
	// (本包不持仓储);此处只判形状。
	if _, ok := parseMediaLeaseRef(image.URL); ok {
		return 0, nil
	}
	const dataPrefix = "data:"
	if !strings.HasPrefix(image.URL, dataPrefix) {
		return 0, errors.New("image_url must be a base64 data URI or a gateway-issued media lease path")
	}
	comma := strings.IndexByte(image.URL, ',')
	if comma < 0 {
		return 0, errors.New("image_url must be a base64 data URI")
	}
	meta, encoded := image.URL[len(dataPrefix):comma], image.URL[comma+1:]
	var magic func([]byte) bool
	switch meta {
	case "image/jpeg;base64":
		magic = isJPEG
	case "image/png;base64":
		magic = isPNG
	case "image/webp;base64":
		magic = isWebP
	default:
		return 0, errors.New("image_url must use image/jpeg, image/png, or image/webp")
	}
	decoded, err := decodeBase64(encoded, maxDecoded)
	if err != nil {
		return 0, err
	}
	if !magic(decoded) {
		return 0, errors.New("image MIME type does not match its data")
	}
	return int64(len(decoded)), nil
}

func validateVideo(video *VideoURL, maxDecoded int64) (int64, error) {
	if video == nil {
		return 0, errors.New("video_url payload is required")
	}
	if _, ok := parseMediaLeaseRef(video.URL); ok {
		return 0, nil // ADR 0011 — see validateImage. 见 validateImage。
	}
	if !strings.HasPrefix(video.URL, "data:video/mp4;base64,") {
		return 0, errors.New("video_url must use an inline video/mp4 base64 data URI or a gateway-issued media lease path")
	}
	decoded, err := decodeBase64(strings.TrimPrefix(video.URL, "data:video/mp4;base64,"), maxDecoded)
	if err != nil {
		return 0, err
	}
	if !isMP4(decoded) {
		return 0, errors.New("video MIME type does not match its data")
	}
	return int64(len(decoded)), nil
}

func validateAudio(audio *InputAudio, maxDecoded int64) (int64, error) {
	if audio == nil {
		return 0, errors.New("input_audio payload is required")
	}
	var magic func([]byte) bool
	switch audio.Format {
	case "wav":
		magic = isWAV
	case "mp3":
		magic = isMP3
	default:
		return 0, errors.New("input_audio format must be wav or mp3")
	}
	decoded, err := decodeBase64(audio.Data, maxDecoded)
	if err != nil {
		return 0, err
	}
	if !magic(decoded) {
		return 0, errors.New("input_audio format does not match its data")
	}
	return int64(len(decoded)), nil
}

func decodeBase64(encoded string, maxDecoded int64) ([]byte, error) {
	if encoded == "" {
		return nil, errors.New("media base64 data is empty")
	}
	decodedLen, ok := strictBase64DecodedLen(encoded)
	if !ok {
		return nil, errors.New("invalid media base64 data")
	}
	if maxDecoded <= 0 || decodedLen > maxDecoded {
		return nil, errors.New("media data exceeds the decoded-byte limit")
	}
	decoded := make([]byte, decodedLen)
	n, err := base64.StdEncoding.Strict().Decode(decoded, []byte(encoded))
	if err != nil || int64(n) != decodedLen {
		return nil, errors.New("invalid media base64 data")
	}
	return decoded[:n], nil
}

func strictBase64DecodedLen(encoded string) (int64, bool) {
	n := len(encoded)
	if n == 0 || n%4 != 0 {
		return 0, false
	}
	padding := 0
	if encoded[n-1] == '=' {
		padding++
		if n >= 2 && encoded[n-2] == '=' {
			padding++
		}
	}
	if strings.Contains(encoded[:n-padding], "=") {
		return 0, false
	}
	return int64(n/4*3 - padding), true
}

func isJPEG(data []byte) bool {
	return len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff
}

func isPNG(data []byte) bool {
	return len(data) >= 8 && bytes.Equal(data[:8], []byte{'\x89', 'P', 'N', 'G', '\r', '\n', '\x1a', '\n'})
}

func isWebP(data []byte) bool {
	return len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP"))
}

func isWAV(data []byte) bool {
	return len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WAVE"))
}

func isMP3(data []byte) bool {
	return len(data) >= 3 && bytes.Equal(data[:3], []byte("ID3")) ||
		len(data) >= 2 && data[0] == 0xff && data[1]&0xe0 == 0xe0
}

func isMP4(data []byte) bool {
	return len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp"))
}

func (c Content) textRunes() int {
	switch c.kind {
	case ContentKindString:
		return utf8.RuneCountInString(c.text)
	case ContentKindParts:
		total := 0
		for _, part := range c.parts {
			if part.Type == PartTypeText {
				total += utf8.RuneCountInString(part.Text)
			}
		}
		return total
	default:
		return 0
	}
}

// estimateBytesAndRunes optionally includes media's encoded representation.
// Text-only routing includes everything. Multimodal context checks exclude the
// base64 payload because Qwen tokenizes the decoded media, while retaining
// its type/MIME/format metadata and all surrounding text.
func (c Content) estimateBytesAndRunes(includeMediaPayload bool) (int64, int64) {
	switch c.kind {
	case ContentKindString:
		return int64(len(c.text)), int64(utf8.RuneCountInString(c.text))
	case ContentKindParts:
		var byteCount, runeCount int64
		for _, part := range c.parts {
			byteCount += int64(len(part.Type))
			runeCount += int64(utf8.RuneCountInString(part.Type))
			switch part.Type {
			case PartTypeText:
				byteCount += int64(len(part.Text))
				runeCount += int64(utf8.RuneCountInString(part.Text))
			case PartTypeImageURL:
				if part.ImageURL != nil {
					url := part.ImageURL.URL
					if !includeMediaPayload {
						if comma := strings.IndexByte(url, ','); comma >= 0 {
							url = url[:comma]
						}
					}
					byteCount += int64(len(url) + len(part.ImageURL.Detail))
					runeCount += int64(utf8.RuneCountInString(url) + utf8.RuneCountInString(part.ImageURL.Detail))
				}
			case PartTypeVideoURL:
				if part.VideoURL != nil {
					url := part.VideoURL.URL
					if !includeMediaPayload {
						if comma := strings.IndexByte(url, ','); comma >= 0 {
							url = url[:comma]
						}
					}
					byteCount += int64(len(url))
					runeCount += int64(utf8.RuneCountInString(url))
				}
			case PartTypeInputAudio:
				if part.InputAudio != nil {
					data := part.InputAudio.Data
					if !includeMediaPayload {
						data = ""
					}
					byteCount += int64(len(data) + len(part.InputAudio.Format))
					runeCount += int64(utf8.RuneCountInString(data) + utf8.RuneCountInString(part.InputAudio.Format))
				}
			}
			// Array/object framing overhead; deliberately favors over-reserve.
			byteCount += 16
			runeCount += 8
		}
		return byteCount, runeCount
	default:
		return 0, 0
	}
}
