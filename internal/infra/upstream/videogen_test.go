package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/domain/apierr"
	domvideo "github.com/sunweilin/anselm/gateway/internal/domain/video"
)

// The video client had no test file at all — the only paid upstream in the repo
// without one. Its 429 charged users for a request the provider had refused, and
// nothing said so; the image client's identical bug at least had a test, whose
// comment argued (wrongly) for the charge. An untested money path does not even
// get to be wrong out loud.
//
// 视频 client 此前**一个测试文件都没有**——本仓唯一一条没有测试的付费上游。它的 429 为一个被上游
// 拒掉的请求向用户收了钱,而没有任何东西说出这件事;图像 client 那个一模一样的缺陷至少还有一个
// 测试,其注释(错误地)在为扣款辩护。一条没有测试的钱路径,连「大声地错」都做不到。

// TestVideoGen_StatusToChargeMapping pins the money half of every status this
// client can see. `unbilled` is the whole point: it decides whether the caller
// rolls the reservation back or keeps the full quote (GW-INV-50).
//
// TestVideoGen_StatusToChargeMapping 钉住本 client 能见到的每个状态码对应的**钱**。`unbilled`
// 就是全部要害:它决定调用方回滚预留还是保留全额报价(GW-INV-50)。
func TestVideoGen_StatusToChargeMapping(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		body         string
		wantCode     string
		wantUnbilled bool
		why          string
	}{
		{"429 refused the request outright", http.StatusTooManyRequests, `{}`,
			"UPSTREAM_BUSY", true,
			"nothing was queued, so nothing was billed"},
		{"400 is a request rejection", http.StatusBadRequest, `{}`,
			"UPSTREAM_REJECTED", true,
			"an explicit refusal of the request never reaches generation"},
		{"422 is a request rejection", http.StatusUnprocessableEntity, `{}`,
			"UPSTREAM_REJECTED", true, "same class as 400"},
		{"500 is ambiguous", http.StatusInternalServerError, `{}`,
			"UPSTREAM_ERROR", false,
			"the provider may have started and billed — keep the full quote"},
		{"a 200 with no task id is ambiguous", http.StatusOK, `{"output":{}}`,
			"UPSTREAM_ERROR", false,
			"a 200 we cannot read may still have queued a paid job"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeDashScope(t, tc.status, tc.body)
			defer srv.Close()
			g := NewVideoGen(srv.URL, "sk")

			_, unbilled, err := g.SubmitVideo(context.Background(), "wan", "p", 5, "16:9", "720P")
			var ae *apierr.APIError
			if !asAPIErr(err, &ae) || ae.Code != tc.wantCode {
				t.Fatalf("err = %v, want %s", err, tc.wantCode)
			}
			if unbilled != tc.wantUnbilled {
				t.Fatalf("unbilled = %v, want %v — %s", unbilled, tc.wantUnbilled, tc.why)
			}
		})
	}
}

// TestVideoGen_SubmitSpeaksTheAsyncWire: the async header is mandatory (without
// it DashScope answers synchronously and the whole handle/poll design collapses),
// the key rides only the Authorization header, and the task id is what comes back.
//
// 异步头是**必需**的(没有它 DashScope 会同步作答,整套句柄/轮询设计随之崩塌),key 只走
// Authorization 头,而回来的是 task id。
func TestVideoGen_SubmitSpeaksTheAsyncWire(t *testing.T) {
	var gotAsync, gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAsync = r.Header.Get("X-DashScope-Async")
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"output":{"task_id":"task-abc","task_status":"PENDING"}}`))
	}))
	defer srv.Close()

	taskID, unbilled, err := NewVideoGen(srv.URL, "sk-secret").
		SubmitVideo(context.Background(), "wan", "a cat", 5, "16:9", "720P")
	if err != nil || taskID != "task-abc" {
		t.Fatalf("SubmitVideo = %q, %v", taskID, err)
	}
	if unbilled {
		t.Fatal("an accepted submission is billed — it must not be refundable")
	}
	if gotAsync != "enable" {
		t.Fatalf("X-DashScope-Async = %q, want enable", gotAsync)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotPath != "/api/v1/services/aigc/video-generation/video-synthesis" {
		t.Fatalf("path = %q", gotPath)
	}
}

func TestVideoGen_SubmitAnimationUsesWan27MediaWire(t *testing.T) {
	var payload struct {
		Model string `json:"model"`
		Input struct {
			Prompt string `json:"prompt"`
			Media  []struct {
				Type string `json:"type"`
				URL  string `json:"url"`
			} `json:"media"`
		} `json:"input"`
		Parameters map[string]any `json:"parameters"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode animation payload: %v", err)
		}
		_, _ = w.Write([]byte(`{"output":{"task_id":"task-i2v"}}`))
	}))
	defer srv.Close()

	const frame = "data:image/png;base64,iVBORw0KGgo="
	taskID, _, err := NewVideoGen(srv.URL, "sk").SubmitAnimation(
		context.Background(), "wan2.7-i2v-2026-04-25", "move naturally", 5, "720P", frame)
	if err != nil || taskID != "task-i2v" {
		t.Fatalf("SubmitAnimation = %q, %v", taskID, err)
	}
	if payload.Model != "wan2.7-i2v-2026-04-25" || payload.Input.Prompt != "move naturally" {
		t.Fatalf("identity = model %q prompt %q", payload.Model, payload.Input.Prompt)
	}
	if len(payload.Input.Media) != 1 || payload.Input.Media[0].Type != "first_frame" || payload.Input.Media[0].URL != frame {
		t.Fatalf("media = %#v, want one typed first_frame", payload.Input.Media)
	}
	if payload.Parameters["resolution"] != "720P" || payload.Parameters["duration"] != float64(5) {
		t.Fatalf("parameters = %#v", payload.Parameters)
	}
	if _, leaked := payload.Parameters["ratio"]; leaked {
		t.Fatalf("animation must inherit the first frame ratio: %#v", payload.Parameters)
	}
}

// TestVideoGen_PollMapsPhasesAndForgottenTasks: polling is the one verb that
// touches no money, so what it must get right is the CLOSED phase vocabulary and
// the 404 that means "the provider forgot this task", not "your signature failed".
//
// 轮询是唯一不碰钱的动词,故它必须做对的是**封闭的**状态词表,以及那个意为「上游忘了这个任务」
// (而非「你的签名不对」)的 404。
func TestVideoGen_PollMapsPhasesAndForgottenTasks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		want   domvideo.Phase
		hasURL bool
	}{
		{"pending", `{"output":{"task_status":"PENDING"}}`, domvideo.PhasePending, false},
		{"running", `{"output":{"task_status":"RUNNING"}}`, domvideo.PhaseRunning, false},
		{"failed", `{"output":{"task_status":"FAILED"}}`, domvideo.PhaseFailed, false},
		{"succeeded carries the artifact",
			`{"output":{"task_status":"SUCCEEDED","video_url":"https://oss.example.com/v.mp4?sig=s"}}`,
			domvideo.PhaseSucceeded, true},
		// An unknown vendor status reports RUNNING, never FAILED: the closed set
		// belongs to the vendor, and a new member must not turn every healthy job
		// into an error at once.
		// 未知的厂商状态报 RUNNING、绝不报 FAILED:封闭集是厂商的,新成员不该在同一瞬间把所有
		// 健康任务变成错误。
		{"an unknown status degrades to running", `{"output":{"task_status":"QUEUING"}}`,
			domvideo.PhaseRunning, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeDashScope(t, http.StatusOK, tc.body)
			defer srv.Close()
			st, err := NewVideoGen(srv.URL, "sk").PollVideo(context.Background(), "task-abc")
			if err != nil {
				t.Fatalf("PollVideo: %v", err)
			}
			if st.Phase != tc.want {
				t.Fatalf("phase = %q, want %q", st.Phase, tc.want)
			}
			if (st.URL != "") != tc.hasURL {
				t.Fatalf("url = %q, want present=%v", st.URL, tc.hasURL)
			}
		})
	}

	t.Run("succeeded with nothing fetchable is a contract break", func(t *testing.T) {
		srv := fakeDashScope(t, http.StatusOK, `{"output":{"task_status":"SUCCEEDED","video_url":"http://oss.example.com/v.mp4"}}`)
		defer srv.Close()
		_, err := NewVideoGen(srv.URL, "sk").PollVideo(context.Background(), "task-abc")
		var ae *apierr.APIError
		if !asAPIErr(err, &ae) || ae.Code != "UPSTREAM_ERROR" {
			t.Fatalf("a plaintext artifact URL must not reach the client, got %v", err)
		}
	})

	t.Run("a forgotten task is not an upstream error", func(t *testing.T) {
		srv := fakeDashScope(t, http.StatusNotFound, `{}`)
		defer srv.Close()
		_, err := NewVideoGen(srv.URL, "sk").PollVideo(context.Background(), "task-gone")
		var ae *apierr.APIError
		if !asAPIErr(err, &ae) || ae.Code != "VIDEO_TASK_NOT_FOUND" {
			t.Fatalf("err = %v, want VIDEO_TASK_NOT_FOUND", err)
		}
	})
}

// TestVideoGen_UnconfiguredNeverLeavesTheProcess: with no origin or key there is
// nothing to call, and the caller must learn that before it can charge anybody.
//
// 没有 origin 或 key 时无处可打,而调用方必须在向任何人收费**之前**就知道这件事。
func TestVideoGen_Unconfigured(t *testing.T) {
	_, _, err := NewVideoGen("", "").SubmitVideo(context.Background(), "wan", "p", 5, "", "")
	var ae *apierr.APIError
	if !asAPIErr(err, &ae) || ae.Code != "VIDEO_UNAVAILABLE" {
		t.Fatalf("err = %v, want VIDEO_UNAVAILABLE", err)
	}
}
