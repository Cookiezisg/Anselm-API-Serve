// Package video owns the two pure rules the asynchronous video capability needs
// (WRK-082 H1): the closed phase vocabulary the wire reports, and the SIGNED
// HANDLE that binds one upstream task to the install that paid for it.
//
// Video is the gateway's only two-request capability. Everything else here
// (chat, image, speech) begins and ends inside one HTTP request, so "who is
// allowed to read this result" never had to be asked. Video submits, then the
// desktop polls minutes later — and between those two requests the answer has to
// be carried by something.
//
// video 包持有异步视频能力所需的两条**纯**规则(H1):线缆上报的封闭状态词表,以及把一个上游任务
// 绑到**为它付过钱**的那个 install 上的**签名句柄**。
//
// 视频是本网关唯一的**两次请求**能力。别的(chat/image/speech)都在一次 HTTP 请求里开始并结束,
// 所以「谁有权读这个结果」这个问题从来不必问。视频先提交,桌面端几分钟后才轮询——而这两次请求之间,
// 那个答案必须由**某个东西**带着。
package video

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// Phase is the closed status vocabulary this gateway reports. It is deliberately
// NOT the provider's vocabulary: DashScope says PENDING/RUNNING/SUCCEEDED/
// FAILED/CANCELED/UNKNOWN, and re-exporting six vendor words would make the
// desktop's switch statement a hostage of Alibaba's release notes.
//
// Phase 是本网关上报的封闭状态词表。刻意**不是**上游的词表:DashScope 说
// PENDING/RUNNING/SUCCEEDED/FAILED/CANCELED/UNKNOWN,把六个厂商词原样转出去,等于让桌面端的
// switch 成为阿里发版说明的人质。
type Phase string

const (
	PhasePending   Phase = "pending"
	PhaseRunning   Phase = "running"
	PhaseSucceeded Phase = "succeeded"
	PhaseFailed    Phase = "failed"
)

// ErrHandleInvalid — the handle is malformed, or it is well-formed but was
// signed for a DIFFERENT install. Both answer the same on the wire on purpose:
// distinguishing them would tell a caller that some other install owns a task,
// which is exactly the fact the signature exists to withhold.
//
// ErrHandleInvalid —— 句柄畸形,**或**格式正确但签给了**另一个** install。两者在线缆上刻意同答:
// 区分它们等于告诉调用方「有别的 install 拥有这个任务」,而那正是签名存在的目的所要藏住的事实。
var ErrHandleInvalid = errors.New("video: handle is not valid for this install")

// handleSep separates the opaque task id from its tag. The task id is base64url
// encoded first, so a provider id containing this byte can never split the
// handle in the wrong place.
//
// handleSep 分隔不透明 task id 与其 tag。task id 先经 base64url 编码,故上游 id 里含有这个字节
// 也绝不可能把句柄从错误的地方切开。
const handleSep = "."

// tagBytes is how much of the HMAC travels. 16 bytes (128 bits) is far past any
// forgery budget for a value that is worthless the moment its task expires, and
// keeps the handle short enough to sit in a URL path comfortably.
//
// tagBytes 是随句柄旅行的 HMAC 长度。对一个「任务一过期就一文不值」的值来说,16 字节(128 位)
// 远超任何伪造预算,同时让句柄短到能舒服地待在 URL 路径里。
const tagBytes = 16

// DeriveKey produces the handle-signing key from whatever secret material the
// deployment already has, with DOMAIN SEPARATION. It exists so enabling video
// costs the operator no new secret: a gateway that already signs media leases
// can sign video handles, and a leak of one key still cannot forge the other.
//
// DeriveKey 从本部署**已有**的秘密材料派生句柄签名密钥,并做**域分离**。它的存在是为了让开视频
// 不必让运营者新配一个 secret:一个已经在签 media lease 的网关就能签视频句柄,而其中一把密钥泄露
// 仍然伪造不出另一把。
func DeriveKey(material []byte) []byte {
	if len(material) == 0 {
		return nil
	}
	mac := hmac.New(sha256.New, material)
	_, _ = mac.Write([]byte("anselm-gateway-video-handle-v1"))
	return mac.Sum(nil)
}

// SignHandle mints the client-facing id for one accepted submission. The install
// id is inside the MAC but NOT inside the handle: the desktop already knows
// which install it is, and putting it on the wire would only publish it.
//
// SignHandle 为一次被受理的提交铸出面向客户端的 id。install id 在 MAC **之内**、却**不在**句柄
// 之内:桌面端本来就知道自己是哪个 install,把它放上线缆只是白白公开它。
func SignHandle(key []byte, installID, taskID string) string {
	if len(key) == 0 || installID == "" || taskID == "" {
		return ""
	}
	body := base64.RawURLEncoding.EncodeToString([]byte(taskID))
	return body + handleSep + base64.RawURLEncoding.EncodeToString(tag(key, installID, body))
}

// ParseHandle recovers the upstream task id, or refuses. Constant-time compare:
// this is an authorization check, and a timing oracle on the tag would hand an
// attacker the forgery byte by byte.
//
// ParseHandle 取回上游 task id,或者拒绝。**常数时间**比较:这是一次鉴权检查,tag 上的时序侧信道
// 会把伪造一个字节一个字节地送给攻击者。
func ParseHandle(key []byte, installID, handle string) (string, error) {
	if len(key) == 0 || installID == "" {
		return "", ErrHandleInvalid
	}
	body, got, ok := strings.Cut(handle, handleSep)
	if !ok || body == "" || got == "" {
		return "", ErrHandleInvalid
	}
	gotTag, err := base64.RawURLEncoding.DecodeString(got)
	if err != nil || !hmac.Equal(gotTag, tag(key, installID, body)) {
		return "", ErrHandleInvalid
	}
	taskID, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || len(taskID) == 0 {
		return "", ErrHandleInvalid
	}
	return string(taskID), nil
}

// tag binds install and task together under one MAC. The install id is length-
// prefixed by the separator so that ("ab","c") and ("a","bc") cannot collide.
//
// tag 把 install 与 task 绑在同一个 MAC 下。install id 以分隔符定界,使 ("ab","c") 与 ("a","bc")
// 不可能碰撞。
func tag(key []byte, installID, body string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(installID))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(body))
	return mac.Sum(nil)[:tagBytes]
}
