package chat

import "testing"

// The security core of ADR 0011: the accepted shape is EXACTLY the gateway's own relative lease
// fetch path. Every hostile variant below must be rejected — most importantly anything carrying a
// host, because a host that survives here becomes an SSRF executed by the upstream provider.
//
// ADR 0011 的安全内核:被接受的形状**恰好**是网关自己的相对 lease fetch 路径。下列每一种敌意变体都必须
// 被拒——**尤其是任何带 host 的**:host 一旦在此存活,就变成由上游 provider 代为执行的 SSRF。
func TestParseMediaLeaseRef_AcceptsOnlyOwnRelativePath(t *testing.T) {
	ref, ok := parseMediaLeaseRef("/v1/media/leases/mls_abc123/content?token=t0k3n")
	if !ok {
		t.Fatal("the gateway's own relative fetch path must be accepted")
	}
	if ref.LeaseID != "mls_abc123" || ref.Token != "t0k3n" {
		t.Fatalf("id/token must be extracted verbatim, got %+v", ref)
	}

	hostile := map[string]string{
		"absolute https to our own host": "https://api.example.com/v1/media/leases/mls_a/content?token=t",
		"absolute https to another host": "https://evil.example/v1/media/leases/mls_a/content?token=t",
		"protocol-relative host":         "//evil.example/v1/media/leases/mls_a/content?token=t",
		"http scheme":                    "http://evil.example/v1/media/leases/mls_a/content?token=t",
		"userinfo host":                  "https://user@evil.example/v1/media/leases/mls_a/content?token=t",
		"traversal out of the prefix":    "/v1/media/leases/../../secret/content?token=t",
		"nested id segment":              "/v1/media/leases/a/b/content?token=t",
		"empty id":                       "/v1/media/leases//content?token=t",
		"missing token":                  "/v1/media/leases/mls_a/content",
		"empty token":                    "/v1/media/leases/mls_a/content?token=",
		"wrong suffix":                   "/v1/media/leases/mls_a/bytes?token=t",
		"wrong prefix":                   "/v1/media/uploads/mls_a/content?token=t",
		"data uri":                       "data:image/png;base64,AAAA",
		"empty":                          "",
	}
	for name, raw := range hostile {
		if _, ok := parseMediaLeaseRef(raw); ok {
			t.Errorf("%s must be rejected, but parsed: %q", name, raw)
		}
	}
}

// A lease reference must pass content validation while contributing ZERO decoded bytes: the media
// never traverses the chat body — that is the entire point of the one-shot upload. A data URI in
// the same request must still be counted, so the byte guard keeps working for inline media.
//
// lease 引用必须通过内容校验且计**零**解码字节:媒体从不经过 chat body——这正是一次性上传的全部目的。
// 同一请求里的 data URI 仍须计数,使字节护栏对内联媒体照常生效。
func TestValidateImage_LeaseRefCostsNoDecodedBytes(t *testing.T) {
	n, err := validateImage(&ImageURL{URL: "/v1/media/leases/mls_a/content?token=t"}, 0)
	if err != nil {
		t.Fatalf("a lease ref must validate even with zero remaining byte budget: %v", err)
	}
	if n != 0 {
		t.Fatalf("a lease ref must cost 0 decoded bytes, got %d", n)
	}

	if _, err := validateVideo(&VideoURL{URL: "/v1/media/leases/mls_v/content?token=t"}, 0); err != nil {
		t.Fatalf("a video lease ref must validate: %v", err)
	}

	// A host-bearing URL must still be refused by validation itself, not merely by the parser.
	// 带 host 的 URL 必须被**校验本身**拒绝,而不只是解析器不认。
	if _, err := validateImage(&ImageURL{URL: "https://evil.example/v1/media/leases/mls_a/content?token=t"}, 1<<20); err == nil {
		t.Fatal("an absolute URL must never pass image validation")
	}
}

// MediaLeaseRefs must surface every reference the app layer has to verify — in wire order, and only
// from media parts. Missing one would mean handing an unverified reference to a provider.
//
// MediaLeaseRefs 必须交出 app 层需要校验的**每一个**引用(按线缆序、仅取媒体部件)。漏掉一个就等于把未经
// 校验的引用交给 provider。
func TestMediaLeaseRefs_CollectsEveryReferenceInOrder(t *testing.T) {
	in := InboundRequest{Messages: []Message{
		{Role: "user", Content: PartsContent([]ContentPart{
			{Type: PartTypeText, Text: "look"},
			{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "/v1/media/leases/mls_1/content?token=a"}},
			{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:image/png;base64,AAAA"}},
			{Type: PartTypeVideoURL, VideoURL: &VideoURL{URL: "/v1/media/leases/mls_2/content?token=b"}},
		})},
		{Role: "assistant", Content: StringContent("ok")},
		{Role: "user", Content: PartsContent([]ContentPart{
			{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "/v1/media/leases/mls_3/content?token=c"}},
		})},
	}}
	refs := in.MediaLeaseRefs()
	want := []MediaLeaseRef{
		{LeaseID: "mls_1", Token: "a", Raw: "/v1/media/leases/mls_1/content?token=a"},
		{LeaseID: "mls_2", Token: "b", Raw: "/v1/media/leases/mls_2/content?token=b"},
		{LeaseID: "mls_3", Token: "c", Raw: "/v1/media/leases/mls_3/content?token=c"},
	}
	if len(refs) != len(want) {
		t.Fatalf("want %d refs, got %d: %+v", len(want), len(refs), refs)
	}
	for i := range want {
		if refs[i] != want[i] {
			t.Errorf("ref %d: want %+v got %+v", i, want[i], refs[i])
		}
	}
}
