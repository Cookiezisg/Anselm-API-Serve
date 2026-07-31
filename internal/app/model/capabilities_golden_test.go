package model

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/domain/config"
)

// The published capability surface, frozen.
//
// `GET /v1/models` is the ONE place the desktop client learns what it may ask
// for and how much headroom it has. Every other contract in this repo is
// verified by a request failing; this one is verified by a client quietly
// choosing wrong — an over-large budget, or an image picker offered on a
// deployment with no media path.
//
// So it is pinned as bytes. A refactor that preserves behavior leaves this file
// untouched, and a deliberate change to the surface shows up as a reviewable
// diff instead of as a desktop bug three releases later.
//
//	go test ./internal/app/model -run TestCapabilitiesMatchGolden -update-golden
//
// 已发布的能力面,冻结。
//
// `GET /v1/models` 是桌面端**唯一**得知「能要什么、还有多少余量」的地方。本仓其它契约都靠「请求
// 失败」来验证;这一个只会靠「客户端悄悄选错」来暴露——一个过大的预算,或者在根本没有媒体通道的
// 部署上给出一个图片选择器。
//
// 故它按**字节**钉死。行为保持的重构不会碰这个文件;而对能力面的刻意改动会以一份可审阅的 diff
// 出现,而不是三个版本之后的一个桌面端 bug。
var updateGolden = flag.Bool("update-golden", false, "rewrite the capability golden file")

const goldenPath = "testdata/capabilities.golden.json"

// productionShapedConfig mirrors what deploy/build-stage.sh writes: every
// capability on, MAX_TOKENS_CAP at the production value. It is spelled out here
// rather than read from the deploy script on purpose — the golden must not move
// silently because a deployment default moved.
//
// 这份配置照抄 deploy/build-stage.sh 写下的形状:能力全开、MAX_TOKENS_CAP 取生产值。它**刻意
// 写死**而不是从部署脚本读取——golden 不该因为某个部署默认值变了就悄悄跟着动。
func productionShapedConfig() *config.Config {
	return &config.Config{
		PublicModelID:    "anselm-auto",
		MaxTokensCap:     16384,
		QwenAPIKeys:      []string{"present"},
		MediaEnabled:     true,
		ImageEnabled:     true,
		ImageDailyLimit:  10,
		SpeechEnabled:    true,
		SpeechDailyLimit: 50000,
		VideoEnabled:     true,
		VideoDailyLimit:  10,
		VoiceDailyLimit:  2,
		// Video needs a SECOND half: the handle key derived from the media signing
		// secret. Production has it, so the fixture must too — the first draft of
		// this file omitted it and froze `video_generation.available:false`, a shape
		// production never serves. A golden built from a fixture that does not exist
		// is worse than no golden: it passes forever while describing nothing.
		// 视频需要**第二半**:由媒体签名机密派生的句柄密钥。生产上有,故夹具也必须有——本文件的
		// 初稿漏了它,于是冻结了一个 `video_generation.available:false` 的形状,而生产从不提供那个
		// 形状。用一个**不存在的**夹具建出来的 golden 比没有 golden 更糟:它会永远通过,却什么也没
		// 描述。
		VideoHandleKey: []byte("derived-from-media-signing-secret"),
	}
}

func renderCapabilities(t *testing.T) string {
	t.Helper()
	env := New(&fakeCfg{c: productionShapedConfig()}).List()
	raw, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		t.Fatalf("marshal model list: %v", err)
	}
	return string(raw) + "\n"
}

func TestCapabilitiesMatchGolden(t *testing.T) {
	got := renderCapabilities(t)

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o750); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("capability golden rewritten (%d bytes)", len(got))
		return
	}

	want, err := os.ReadFile(goldenPath) // #nosec G304 — fixed repo-relative path
	if err != nil {
		t.Fatalf("read golden (regenerate with -update-golden): %v", err)
	}
	if got != string(want) {
		t.Errorf("the published capability surface changed.\n"+
			"If that is intended, re-approve it explicitly:\n"+
			"  go test ./internal/app/model -run TestCapabilitiesMatchGolden -update-golden\n\n"+
			"--- golden ---\n%s\n--- live ---\n%s", want, got)
	}
}
