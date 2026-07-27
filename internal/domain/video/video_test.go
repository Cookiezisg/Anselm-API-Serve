package video

import (
	"errors"
	"strings"
	"testing"
)

func key(t *testing.T, material string) []byte {
	t.Helper()
	k := DeriveKey([]byte(material))
	if len(k) == 0 {
		t.Fatalf("DeriveKey(%q) returned nothing", material)
	}
	return k
}

func TestHandleRoundTrips(t *testing.T) {
	t.Parallel()
	k := key(t, "media-signing-secret-at-least-32-bytes!!")
	for _, taskID := range []string{
		"b1f2c3d4-1111-2222-3333-444455556666",
		"task.with.dots",
		"task/with/slashes+and=padding",
		"任务",
	} {
		h := SignHandle(k, "ins_abc", taskID)
		if h == "" {
			t.Fatalf("SignHandle produced nothing for %q", taskID)
		}
		got, err := ParseHandle(k, "ins_abc", h)
		if err != nil || got != taskID {
			t.Fatalf("round trip %q → %q, %v", taskID, got, err)
		}
	}
}

// The whole point of the signature: install B must not be able to read install
// A's task, even holding A's handle verbatim.
//
// 签名的全部意义:install B 拿着 A 的句柄**原文**,也读不到 A 的任务。
func TestHandleIsBoundToItsInstall(t *testing.T) {
	t.Parallel()
	k := key(t, "media-signing-secret-at-least-32-bytes!!")
	h := SignHandle(k, "ins_alice", "task-1")
	if _, err := ParseHandle(k, "ins_bob", h); !errors.Is(err, ErrHandleInvalid) {
		t.Fatalf("bob parsed alice's handle: %v", err)
	}
}

// Domain separation: the raw media secret must not itself verify video handles,
// or a leak of one signing surface would forge the other.
//
// 域分离:media 原始 secret 本身不得能验证视频句柄,否则一处签名面泄露即可伪造另一处。
func TestHandleKeyIsDomainSeparated(t *testing.T) {
	t.Parallel()
	material := []byte("media-signing-secret-at-least-32-bytes!!")
	derived := DeriveKey(material)
	h := SignHandle(derived, "ins_abc", "task-1")
	if _, err := ParseHandle(material, "ins_abc", h); !errors.Is(err, ErrHandleInvalid) {
		t.Fatal("the raw media secret verified a video handle — no domain separation")
	}
	if _, err := ParseHandle(DeriveKey([]byte("a different secret entirely!!!!!!")), "ins_abc", h); !errors.Is(err, ErrHandleInvalid) {
		t.Fatal("a different secret verified the handle")
	}
}

func TestHandleRejectsMalformed(t *testing.T) {
	t.Parallel()
	k := key(t, "media-signing-secret-at-least-32-bytes!!")
	good := SignHandle(k, "ins_abc", "task-1")
	body, tagPart, _ := strings.Cut(good, handleSep)
	cases := map[string]string{
		"empty":           "",
		"no separator":    body,
		"empty body":      handleSep + tagPart,
		"empty tag":       body + handleSep,
		"tag not base64":  body + handleSep + "!!!!",
		"body not base64": "!!!!" + handleSep + tagPart,
		"truncated tag":   body + handleSep + tagPart[:len(tagPart)-2],
		"swapped halves":  tagPart + handleSep + body,
	}
	for name, h := range cases {
		if _, err := ParseHandle(k, "ins_abc", h); !errors.Is(err, ErrHandleInvalid) {
			t.Fatalf("%s: accepted %q", name, h)
		}
	}
}

// A missing key must refuse rather than sign with nothing — an all-empty key
// would make every handle verify against every install.
//
// 没有 key 必须拒绝、而不是拿空的去签——空 key 会让每个句柄对每个 install 都验得过。
func TestHandleRefusesWithoutKey(t *testing.T) {
	t.Parallel()
	if h := SignHandle(nil, "ins_abc", "task-1"); h != "" {
		t.Fatalf("signed without a key: %q", h)
	}
	if _, err := ParseHandle(nil, "ins_abc", "anything.anything"); !errors.Is(err, ErrHandleInvalid) {
		t.Fatal("parsed without a key")
	}
	if k := DeriveKey(nil); len(k) != 0 {
		t.Fatal("derived a key from no material")
	}
}

// The separator must not let ("ab","c") and ("a","bc") collide into one tag.
//
// 分隔符必须让 ("ab","c") 与 ("a","bc") 不可能碰撞成同一个 tag。
func TestHandleTagHasNoBoundaryCollision(t *testing.T) {
	t.Parallel()
	k := key(t, "media-signing-secret-at-least-32-bytes!!")
	h := SignHandle(k, "ins_ab", "c")
	if _, err := ParseHandle(k, "ins_a", h); !errors.Is(err, ErrHandleInvalid) {
		t.Fatal("install boundary collision")
	}
}
