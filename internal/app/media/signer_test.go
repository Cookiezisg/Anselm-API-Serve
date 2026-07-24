package media

import (
	"testing"
	"time"
)

func TestHMACSignerBindsLeaseInstallAndExpiry(t *testing.T) {
	s, err := NewHMACSigner([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Date(2026, 7, 24, 1, 2, 3, 0, time.UTC)
	token := s.Token("mls_one", "ins_one", expires)
	if !s.Verify(token, "mls_one", "ins_one", expires) {
		t.Fatal("valid token rejected")
	}
	if s.Verify(token, "mls_two", "ins_one", expires) || s.Verify(token, "mls_one", "ins_two", expires) || s.Verify(token, "mls_one", "ins_one", expires.Add(time.Second)) {
		t.Fatal("token must be bound to its exact durable lease facts")
	}
}

func TestHMACSignerRejectsWeakSecret(t *testing.T) {
	if _, err := NewHMACSigner([]byte("too short")); err == nil {
		t.Fatal("weak secret accepted")
	}
}
