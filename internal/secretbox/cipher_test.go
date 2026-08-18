package secretbox

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	key := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	cipher, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("https://example.test/hook")
	if err != nil {
		t.Fatal(err)
	}
	if encrypted == "https://example.test/hook" {
		t.Fatal("secret was not encrypted")
	}
	plain, err := cipher.Decrypt(encrypted)
	if err != nil || plain != "https://example.test/hook" {
		t.Fatalf("plain=%q err=%v", plain, err)
	}
}
