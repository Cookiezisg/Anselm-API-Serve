package deviceproof

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/pkg/noncecache"
)

type keyResolver struct{ key []byte }

func (r keyResolver) PublicKey(context.Context, string) ([]byte, bool, error) {
	return r.key, true, nil
}

func TestVerifyInstallBindsRequestAndRejectsReplay(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	replay := noncecache.New(2 * time.Minute)
	replay.SetNow(func() time.Time { return now })
	svc := New(keyResolver{key: public}, replay)
	svc.now = func() time.Time { return now }
	req := Request{Method: "POST", Target: "/v1/chat/completions", Body: []byte(`{"hello":"world"}`)}
	proof := signTestProof(t, svc, private, "ins_test", "jti-1", req, now)
	if ae := svc.VerifyInstall(context.Background(), req, "ins_test", proof); ae != nil {
		t.Fatalf("valid proof rejected: %v", ae)
	}
	if ae := svc.VerifyInstall(context.Background(), req, "ins_test", proof); ae == nil || ae.Code != "DEVICE_PROOF_REPLAYED" {
		t.Fatalf("replay = %v", ae)
	}
	mutated := req
	mutated.Body = []byte(`{"hello":"attacker"}`)
	proof2 := signTestProof(t, svc, private, "ins_test", "jti-2", req, now)
	if ae := svc.VerifyInstall(context.Background(), mutated, "ins_test", proof2); ae == nil || ae.Code != "DEVICE_PROOF_INVALID" {
		t.Fatalf("mutated body = %v", ae)
	}
}

func TestVerifyRegistrationReturnsKeyThumbprint(t *testing.T) {
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	replay := noncecache.New(2 * time.Minute)
	replay.SetNow(func() time.Time { return now })
	svc := New(nil, replay)
	svc.now = func() time.Time { return now }
	req := Request{Method: "POST", Target: "/v1/install", Body: []byte(`{}`)}
	proof := signTestProof(t, svc, private, Thumbprint(public), "register-1", req, now)
	reg, ae := svc.VerifyRegistration(req, b64.EncodeToString(public), proof)
	if ae != nil {
		t.Fatal(ae)
	}
	if reg.Thumbprint != Thumbprint(public) || string(reg.PublicKey) != string(public) {
		t.Fatalf("registration = %+v", reg)
	}
}

func TestVerifyInstallRejectsEveryBoundFieldAndStaleTimestamp(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	base := Request{Method: "POST", Target: "api.example/v1/chat/completions?x=1", Body: []byte(`{"hello":"world"}`)}

	tests := []struct {
		name      string
		proofKid  string
		proofTime time.Time
		verifyReq Request
	}{
		{name: "method", proofKid: "ins_test", proofTime: now, verifyReq: Request{Method: "PUT", Target: base.Target, Body: base.Body}},
		{name: "target", proofKid: "ins_test", proofTime: now, verifyReq: Request{Method: base.Method, Target: "api.example/v1/quota", Body: base.Body}},
		{name: "body", proofKid: "ins_test", proofTime: now, verifyReq: Request{Method: base.Method, Target: base.Target, Body: []byte(`{"hello":"tampered"}`)}},
		{name: "key id", proofKid: "ins_other", proofTime: now, verifyReq: base},
		{name: "stale timestamp", proofKid: "ins_test", proofTime: now.Add(-proofSkew - time.Second), verifyReq: base},
		{name: "future timestamp", proofKid: "ins_test", proofTime: now.Add(proofSkew + time.Second), verifyReq: base},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			replay := noncecache.New(2 * time.Minute)
			replay.SetNow(func() time.Time { return now })
			svc := New(keyResolver{key: public}, replay)
			svc.now = func() time.Time { return now }
			proof := signTestProof(t, svc, private, tt.proofKid, "bound-"+string(rune('a'+i)), base, tt.proofTime)
			if ae := svc.VerifyInstall(context.Background(), tt.verifyReq, "ins_test", proof); ae == nil || ae.Code != "DEVICE_PROOF_INVALID" {
				t.Fatalf("VerifyInstall = %v, want DEVICE_PROOF_INVALID", ae)
			}
		})
	}
}

func TestVerifyInstallReturnsDistinctExpiredNonceError(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	replay := noncecache.New(2 * time.Minute)
	replay.SetNow(func() time.Time { return now })
	svc := New(keyResolver{key: public}, replay)
	svc.now = func() time.Time { return now }
	req := Request{Method: "GET", Target: "api.example/v1/quota"}
	challenge := svc.IssueChallenge()

	now = now.Add(nonceTTL)
	proof := signPayload(t, private, payload{
		Version: 1, KeyID: "ins_test", Issued: now.Unix(), ID: "expired-nonce",
		Nonce: challenge.Nonce, Method: req.Method, Target: req.Target, Body: bodyHash(req.Body),
	})
	if ae := svc.VerifyInstall(context.Background(), req, "ins_test", proof); ae == nil || ae.Code != "DEVICE_PROOF_NONCE_INVALID" {
		t.Fatalf("VerifyInstall = %v, want DEVICE_PROOF_NONCE_INVALID", ae)
	}
}

func TestOversizedProofAndIdentityAreRejectedBeforeLookup(t *testing.T) {
	svc := New(keyResolver{}, noncecache.New(time.Minute))
	if ae := svc.VerifyInstall(context.Background(), Request{}, strings.Repeat("i", maxInstallID+1), "x.y"); ae == nil || ae.Code != "INVALID_INSTALL" {
		t.Fatalf("oversized install id = %v", ae)
	}
	if ae := svc.verify(Request{}, "id", make(ed25519.PublicKey, ed25519.PublicKeySize), strings.Repeat("x", maxProofBytes+1)); ae == nil || ae.Code != "DEVICE_PROOF_INVALID" {
		t.Fatalf("oversized proof = %v", ae)
	}
}

func signTestProof(t *testing.T, svc *Service, private ed25519.PrivateKey, kid, jti string, req Request, now time.Time) string {
	t.Helper()
	challenge := svc.IssueChallenge()
	return signPayload(t, private, payload{
		Version: 1, KeyID: kid, Issued: now.Unix(), ID: jti, Nonce: challenge.Nonce,
		Method: req.Method, Target: req.Target, Body: bodyHash(req.Body),
	})
}

func signPayload(t *testing.T, private ed25519.PrivateKey, p payload) string {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	encoded := b64.EncodeToString(raw)
	return encoded + "." + b64.EncodeToString(ed25519.Sign(private, []byte(encoded)))
}

func bodyHash(body []byte) string {
	bh := sha256.Sum256(body)
	return b64.EncodeToString(bh[:])
}
