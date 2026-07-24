package media

import "testing"

func TestValidSHA256AndStateClosures(t *testing.T) {
	good := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if !ValidSHA256(good) || ValidSHA256(good[:63]) || ValidSHA256("G"+good[1:]) {
		t.Fatal("sha256 validation must be exact lowercase hexadecimal")
	}
	if !ValidUploadState(UploadOpen) || ValidUploadState("running") {
		t.Fatal("upload state closure is open")
	}
	if !ValidLeaseState(LeaseActive) || ValidLeaseState("consumed") {
		t.Fatal("lease state closure is open")
	}
}

func TestHashSecretNeverReturnsPlaintext(t *testing.T) {
	const secret = "opaque-provider-fetch-capability"
	if got := HashSecret(secret); got == secret || len(got) != 64 || got != HashSecret(secret) {
		t.Fatalf("secret hash invariant failed: %q", got)
	}
}
