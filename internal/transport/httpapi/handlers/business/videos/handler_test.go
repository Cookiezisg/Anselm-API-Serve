package videos

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The shape gate must refuse BEFORE the service is reached — a malformed request
// that reserves first would spend one of the day's ten clips on a 400.
//
// 形状闸必须在**到达服务之前**就拒——一个先预留再失败的畸形请求,会把当天十条里的一条花在一个 400 上。
func TestGenerateRefusesBadShapeWithoutTouchingTheService(t *testing.T) {
	t.Parallel()
	// A nil service is the assertion: any request that reaches it panics, so
	// every case below proves the handler answered on its own.
	// nil 服务**就是**断言:任何走到它的请求都会 panic,故下面每个用例都证明了 handler 自己作答。
	h := New(nil)
	cases := map[string]string{
		"not json":          `{`,
		"unknown field":     `{"prompt":"cat","nope":1}`,
		"empty prompt":      `{"prompt":"   "}`,
		"missing prompt":    `{"seconds":5}`,
		"prompt too long":   `{"prompt":"` + strings.Repeat("x", maxPromptChars+1) + `"}`,
		"seconds too small": `{"prompt":"cat","seconds":1}`,
		"seconds too big":   `{"prompt":"cat","seconds":16}`,
		"negative seconds":  `{"prompt":"cat","seconds":-5}`,
		"unknown aspect":    `{"prompt":"cat","aspect":"cinemascope"}`,
		"aspect as ratio":   `{"prompt":"cat","aspect":"16:9"}`,
		"unknown quality":   `{"prompt":"cat","resolution":"4k"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.Generate(rec, httptest.NewRequest(http.MethodPost, "/v1/videos/generations", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 (body %s)", rec.Code, body)
			}
		})
	}
}

// An empty path value must answer NOT_FOUND on its own rather than asking the
// service about the empty handle.
//
// 空路径值必须自己答 NOT_FOUND,而不是拿一个空句柄去问服务。
func TestStatusRefusesEmptyHandle(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	New(nil).Status(rec, httptest.NewRequest(http.MethodGet, "/v1/videos/", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != "VIDEO_TASK_NOT_FOUND" {
		t.Fatalf("body = %s (%v)", rec.Body.String(), err)
	}
}

// The aspect and resolution tables are the wire's closed vocabulary; pinning the
// translation keeps a rename from silently sending a shape the provider ignores.
//
// aspect 与 resolution 两张表是线缆的封闭词表;钉死翻译,可防一次改名静默送出上游根本不看的形状。
func TestVocabularyTranslation(t *testing.T) {
	t.Parallel()
	for word, ratio := range map[string]string{"landscape": "16:9", "portrait": "9:16", "square": "1:1"} {
		if aspects[word] != ratio {
			t.Fatalf("aspects[%q] = %q, want %q", word, aspects[word], ratio)
		}
	}
	for word, res := range map[string]string{"720p": "720P", "1080p": "1080P"} {
		if resolutions[word] != res {
			t.Fatalf("resolutions[%q] = %q, want %q", word, resolutions[word], res)
		}
	}
	if len(aspects) != 3 || len(resolutions) != 2 {
		t.Fatal("the vocabularies are closed sets — a new member is legislated, not added")
	}
}
