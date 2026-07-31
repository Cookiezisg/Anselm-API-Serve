package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sunweilin/anselm/gateway/internal/app/deviceproof"
)

// A smoke tool whose signatures do not verify is worse than no smoke tool: every
// endpoint answers 401 and the operator concludes the deployment is broken.
//
// So the proof this tool builds is checked against the REAL verifier — the same
// deviceproof.Service the gateway runs — rather than eyeballed against the e2e
// helper it was ported from. That matters concretely here: the port switched the
// payload from a struct to a map, and Go sorts map keys, so the JSON field order
// differs. It is fine (the server verifies over the received encoded bytes, not a
// re-serialization), but "it is fine" is a claim, and this is the test that
// settles it.
//
// 一个签名验不过的冒烟工具比没有更糟:每条端点都答 401,而运营者会以为是部署坏了。
//
// 故本工具造出的 proof 拿**真的验证器**——网关自己跑的那个 deviceproof.Service——来检,而不是
// 对着它移植自的 e2e helper 目测。这里这一点很具体:移植时把载荷从 struct 换成了 map,而 Go 会
// 对 map 的键排序,于是 JSON 字段顺序不同。这没问题(服务端对**收到的编码字节**验签,不是对重新
// 序列化的结果),但「没问题」是一个断言,而这条测试是了结它的那个东西。

type fixedKey struct{ pub ed25519.PublicKey }

func (f fixedKey) PublicKey(context.Context, string) ([]byte, bool, error) {
	return f.pub, true, nil
}

// allowOnce is the replay guard: a jti is admitted exactly once, which is what
// makes a captured proof single-use.
type allowOnce struct{ seen map[string]bool }

func (a *allowOnce) UseOnce(jti string) bool {
	if a.seen == nil {
		a.seen = map[string]bool{}
	}
	if a.seen[jti] {
		return false
	}
	a.seen[jti] = true
	return true
}

// harness points the tool at a server that ONLY issues nonces, so sign() runs its
// real challenge round-trip against a real IssueChallenge.
func harness(t *testing.T) (*client, *deviceproof.Service) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	svc := deviceproof.New(fixedKey{pub: pub}, &allowOnce{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/proof/challenge" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		ch := svc.IssueChallenge()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"nonce": ch.Nonce})
	}))
	t.Cleanup(srv.Close)

	*base = srv.URL
	return &client{http: srv.Client(), priv: priv, installID: "ins_smoke"}, svc
}

// targetOf recomputes the `htu` the signer bound, so the assertion checks the
// signature rather than accidentally re-deriving it from the same expression.
func targetOf(req *http.Request) string {
	target := strings.ToLower(req.URL.Host) + req.URL.EscapedPath()
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	return target
}

func TestSignedProofVerifiesAgainstTheRealVerifier(t *testing.T) {
	c, svc := harness(t)
	body := []byte(`{"prompt":"a leaf","size":"1024x1024"}`)

	req := c.sign(http.MethodPost, "/v1/images/generations", body, true)

	ae := svc.VerifyInstall(context.Background(), deviceproof.Request{
		Method: req.Method,
		Target: targetOf(req),
		Body:   body,
	}, "ins_smoke", req.Header.Get(headerProof))
	if ae != nil {
		t.Fatalf("the gateway's own verifier rejected our proof: %v", ae)
	}
}

// TestProofIsBoundToTheExactBody: the whole point of `bh` is that a captured
// proof cannot be replayed onto a different request. If this ever passes with a
// mutated body, the tool is signing something other than what it sends.
//
// `bh` 的全部意义就是「截获的 proof 不能被重放到另一个请求上」。若哪天改了 body 它还能过,
// 说明本工具签的东西和它发出去的东西不是同一个。
func TestProofIsBoundToTheExactBody(t *testing.T) {
	c, svc := harness(t)
	body := []byte(`{"prompt":"a leaf"}`)
	req := c.sign(http.MethodPost, "/v1/images/generations", body, true)

	ae := svc.VerifyInstall(context.Background(), deviceproof.Request{
		Method: req.Method,
		Target: targetOf(req),
		Body:   []byte(`{"prompt":"something else"}`),
	}, "ins_smoke", req.Header.Get(headerProof))
	if ae == nil {
		t.Fatal("a proof must not verify against a body it did not cover")
	}
}

// TestRegistrationProofCarriesTheKeyAndItsThumbprint: /v1/install is the one call
// with no install id yet, so it identifies itself by public key and must key the
// proof on that key's thumbprint.
//
// /v1/install 是唯一还没有 install id 的调用,故它用公钥自证,proof 也必须按那把公钥的指纹索引。
func TestRegistrationProofCarriesTheKeyAndItsThumbprint(t *testing.T) {
	c, svc := harness(t)
	body := []byte(`{"fingerprint":"smoke","client":"smoke"}`)

	req := c.sign(http.MethodPost, "/v1/install", body, true)
	encodedKey := req.Header.Get(headerPublicKey)
	if encodedKey == "" {
		t.Fatal("a registration must carry the public key header")
	}

	reg, ae := svc.VerifyRegistration(deviceproof.Request{
		Method: req.Method,
		Target: targetOf(req),
		Body:   body,
	}, encodedKey, req.Header.Get(headerProof))
	if ae != nil {
		t.Fatalf("registration proof rejected: %v", ae)
	}
	if reg.Thumbprint != deviceproof.Thumbprint(c.priv.Public().(ed25519.PublicKey)) {
		t.Fatal("the verifier derived a different thumbprint than the key we sent")
	}
	if req.Header.Get(headerInstallID) != "" {
		t.Fatal("a registration has no install id yet — it must not claim one")
	}
}
