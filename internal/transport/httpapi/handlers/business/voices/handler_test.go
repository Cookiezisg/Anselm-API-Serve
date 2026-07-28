package voices

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domvoice "github.com/sunweilin/anselm/gateway/internal/domain/voice"
)

// The handler's job is to refuse malformed input BEFORE the service can reach a paid upstream, and
// to report the inventory arithmetic the caller needs in order to understand a refusal. Both are
// asserted against a nil service on purpose: reaching it would be a nil deref, so any test that
// passes here proves the guard fired without the use case running.
//
// handler 的活是**在 service 够得到付费上游之前**拒掉畸形输入,以及报出调用方理解一次拒绝所需的库存
// 算术。两者都**刻意**对着一个 nil service 断言:碰到它就是一次 nil 解引用,故这里任何通过的测试都
// 证明了那道闸在用例跑起来之前就响了。

func post(t *testing.T, h http.Handler, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "/v1/voices", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// TestEnroll_RejectsBadShapeBeforeTheService: every one of these must be refused by the handler,
// because the service behind it is nil — a 500 or a panic would mean the guard did not fire.
//
// TestEnroll_RejectsBadShapeBeforeTheService:这里每一条都必须被 handler 拒掉,因为它背后的 service
// 是 nil——一个 500 或一次 panic 都意味着那道闸没响。
func TestEnroll_RejectsBadShapeBeforeTheService(t *testing.T) {
	h := NewEnroll(nil)
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"empty name", `{"name":"  ","audio":"data:audio/wav;base64,AA"}`, http.StatusBadRequest},
		{"missing audio", `{"name":"narrator"}`, http.StatusBadRequest},
		{"unknown field", `{"name":"n","audio":"data:audio/wav;base64,AA","voiceId":"x"}`, http.StatusBadRequest},
		{"not json", `nonsense`, http.StatusBadRequest},
		{"oversize name", `{"name":"` + strings.Repeat("x", maxNameChars+1) + `","audio":"data:audio/wav;base64,AA"}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := post(t, h, http.MethodPost, tc.body).Code; got != tc.want {
				t.Fatalf("code = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestEnroll_OversizeSampleIsRefusedByShapeCode: too much base64 is a sample problem, not a generic
// bad request — the caller has to know WHICH field to fix, and the clip is the expensive one to
// re-record.
//
// TestEnroll_OversizeSampleIsRefusedByShapeCode:太大的 base64 是**样本**的问题、不是泛泛的 bad
// request——调用方得知道该改**哪个**字段,而那段音频正是重录代价最大的那个。
func TestEnroll_OversizeSampleIsRefusedByShapeCode(t *testing.T) {
	body := `{"name":"n","audio":"` + strings.Repeat("A", maxSampleChars+1) + `"}`
	rec := post(t, NewEnroll(nil), http.MethodPost, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "VOICE_SAMPLE_INVALID") {
		t.Fatalf("body must name the sample as the problem: %s", rec.Body.String())
	}
}

// TestDelete_RejectsBlankID: an empty id would otherwise reach the service as a lookup that can
// only miss, spending a round trip to say what the handler already knows.
//
// TestDelete_RejectsBlankID:空 id 否则会作为一次**只可能落空**的查找抵达 service,花一个来回去说一件
// handler 已经知道的事。
func TestDelete_RejectsBlankID(t *testing.T) {
	for _, body := range []string{`{"voiceId":""}`, `{"voiceId":"   "}`, `{}`} {
		if got := post(t, NewDelete(nil), http.MethodPost, body).Code; got != http.StatusBadRequest {
			t.Fatalf("body %s: code = %d", body, got)
		}
	}
}

// TestMethodGuards: each route accepts exactly one method. List is the GET; the other two are POST,
// including delete — the `:action` suffix (N5) is the project's convention and keeps voice ids out
// of URLs, hence out of proxy logs and referrers.
//
// TestMethodGuards:每条路由**恰好**收一个方法。List 是 GET;另两个是 POST,**包括删除**——`:action`
// 后缀(N5)是本项目的约定,且它让音色 id 不进 URL、因而不进代理日志与 referrer。
func TestMethodGuards(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    http.Handler
		bad  string
	}{
		{"list rejects POST", NewList(nil), http.MethodPost},
		{"enroll rejects GET", NewEnroll(nil), http.MethodGet},
		{"delete rejects GET", NewDelete(nil), http.MethodGet},
		{"delete rejects DELETE", NewDelete(nil), http.MethodDelete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := post(t, tc.h, tc.bad, `{}`).Code; got != http.StatusMethodNotAllowed {
				t.Fatalf("code = %d, want 405", got)
			}
		})
	}
}

// TestListResponse_CarriesTheArithmetic: the cap is the whole reason a caller reads this list. A
// response that shows rows without saying how many more are allowed leaves the next enrollment's
// refusal unexplained — and nothing frees a slot with time, so "try later" would be a lie.
//
// TestListResponse_CarriesTheArithmetic:**上限正是调用方来读这个列表的理由**。只给行、不说还能留几个
// 的响应,会让下一次登记的拒绝无从解释——而时间不会腾出位置,故「过会儿再来」是撒谎。
func TestListResponse_CarriesTheArithmetic(t *testing.T) {
	raw, err := json.Marshal(listResponse{
		Voices:    []voiceItem{{VoiceID: "vce_1", Name: "narrator", CreatedAt: 1}},
		Capacity:  domvoice.PerInstallInventory,
		Remaining: domvoice.PerInstallInventory - 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"voices", "capacity", "remaining"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("the list response must carry %q: %s", k, raw)
		}
	}
	// An empty inventory serialises as [] rather than null, so no client has to special-case null.
	// 空库存序列化成 [] 而非 null,使没有客户端需要为 null 写一个分支。
	empty, _ := json.Marshal(listResponse{Voices: []voiceItem{}})
	if !strings.Contains(string(empty), `"voices":[]`) {
		t.Fatalf("empty inventory must serialise as []: %s", empty)
	}
}
