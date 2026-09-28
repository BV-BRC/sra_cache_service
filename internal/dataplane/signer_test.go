package dataplane

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMintVerifyRoundTrips(t *testing.T) {
	s := New("test-secret", "http://sra-cache.example.org:7142")

	signedURL, expires, err := s.Mint("SRR1234567", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !strings.HasPrefix(signedURL, "http://sra-cache.example.org:7142/sra/") {
		t.Fatalf("url = %q, want the /sra/ prefix", signedURL)
	}
	if !strings.HasSuffix(signedURL, "/SRR1234567.sra") {
		t.Fatalf("url = %q, want it to end in /SRR1234567.sra", signedURL)
	}
	if expires.Before(time.Now()) {
		t.Fatalf("expires = %v, want it in the future", expires)
	}

	key, err := keyFromURL(signedURL)
	if err != nil {
		t.Fatalf("extracting key from url: %v", err)
	}
	if err := s.verifyAt("SRR1234567", key, time.Now()); err != nil {
		t.Errorf("verifyAt: %v, want nil (valid key)", err)
	}
}

func TestVerifyTamperedKeyRejected(t *testing.T) {
	s := New("test-secret", "http://example.org")
	signedURL, _, err := s.Mint("SRR1234567", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	key, err := keyFromURL(signedURL)
	if err != nil {
		t.Fatalf("extracting key: %v", err)
	}

	// Flip the last character of the key -- still valid base64url, still the
	// right length, but the signature (or nonce, or expiry) is now wrong.
	tampered := tamperLastChar(key)
	if tampered == key {
		t.Fatal("tamperLastChar produced no change")
	}

	if err := s.verifyAt("SRR1234567", tampered, time.Now()); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("verifyAt(tampered) = %v, want ErrInvalidSignature", err)
	}
}

func TestVerifyExpiredKeyRejected(t *testing.T) {
	s := New("test-secret", "http://example.org")
	signedURL, expires, err := s.Mint("SRR1234567", time.Minute)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	key, err := keyFromURL(signedURL)
	if err != nil {
		t.Fatalf("extracting key: %v", err)
	}

	// One second after expiry.
	after := expires.Add(time.Second)
	if err := s.verifyAt("SRR1234567", key, after); !errors.Is(err, ErrExpired) {
		t.Errorf("verifyAt(expired) = %v, want ErrExpired", err)
	}

	// One second before expiry must still be valid.
	before := expires.Add(-time.Second)
	if err := s.verifyAt("SRR1234567", key, before); err != nil {
		t.Errorf("verifyAt(not yet expired) = %v, want nil", err)
	}
}

// TestVerifyWrongAccessionRejected is the "minted for a different accession"
// case the plan calls out explicitly: a key that is otherwise well-formed
// and correctly signed for SRR1234567 must not verify against SRR7654321.
func TestVerifyWrongAccessionRejected(t *testing.T) {
	s := New("test-secret", "http://example.org")
	signedURL, _, err := s.Mint("SRR1234567", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	key, err := keyFromURL(signedURL)
	if err != nil {
		t.Fatalf("extracting key: %v", err)
	}

	if err := s.verifyAt("SRR7654321", key, time.Now()); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("verifyAt(wrong accession) = %v, want ErrInvalidSignature", err)
	}
}

// TestVerifyDifferentSecretRejected: a key minted by one Signer must not
// verify against a Signer built with a different secret.
func TestVerifyDifferentSecretRejected(t *testing.T) {
	s1 := New("secret-one", "http://example.org")
	s2 := New("secret-two", "http://example.org")

	signedURL, _, err := s1.Mint("SRR1234567", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	key, err := keyFromURL(signedURL)
	if err != nil {
		t.Fatalf("extracting key: %v", err)
	}

	if err := s2.verifyAt("SRR1234567", key, time.Now()); !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("verifyAt(different secret) = %v, want ErrInvalidSignature", err)
	}
}

func TestMintEmptySecretFails(t *testing.T) {
	s := New("", "http://example.org")
	if _, _, err := s.Mint("SRR1234567", time.Hour); !errors.Is(err, ErrEmptySecret) {
		t.Errorf("Mint with empty secret = %v, want ErrEmptySecret", err)
	}
}

func TestVerifyEmptySecretFails(t *testing.T) {
	s := New("", "http://example.org")
	if err := s.verifyAt("SRR1234567", "anything", time.Now()); !errors.Is(err, ErrEmptySecret) {
		t.Errorf("verifyAt with empty secret = %v, want ErrEmptySecret", err)
	}
}

func TestMintEmptyBaseURLFails(t *testing.T) {
	s := New("test-secret", "")
	if _, _, err := s.Mint("SRR1234567", time.Hour); err == nil {
		t.Error("Mint with empty base URL: want an error, got nil")
	}
}

func TestVerifyGarbageKeyRejected(t *testing.T) {
	s := New("test-secret", "http://example.org")
	cases := []string{
		"",
		"not-base64-!!!",
		// Valid base64url, but the decoded content has no colons at all.
		"bm90aGluZ3Rvc3BsaXQ",
	}
	for _, key := range cases {
		if err := s.verifyAt("SRR1234567", key, time.Now()); !errors.Is(err, ErrInvalidSignature) {
			t.Errorf("verifyAt(%q) = %v, want ErrInvalidSignature", key, err)
		}
	}
}

// TestTwoMintsForSameAccessionProduceDifferentKeys asserts the nonce is
// actually doing something -- two Mint calls for the same accession and a
// timestamp precise enough to plausibly collide should still emit distinct
// keys.
func TestTwoMintsForSameAccessionProduceDifferentKeys(t *testing.T) {
	s := New("test-secret", "http://example.org")
	url1, _, err := s.Mint("SRR1234567", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	url2, _, err := s.Mint("SRR1234567", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if url1 == url2 {
		t.Error("two Mint calls for the same accession produced identical URLs")
	}
}

// keyFromURL extracts the {key} path segment from a URL Mint produced, i.e.
// ".../sra/<key>/<accession>.sra".
func keyFromURL(u string) (string, error) {
	i := strings.Index(u, "/sra/")
	if i < 0 {
		return "", errors.New("no /sra/ segment in url")
	}
	rest := u[i+len("/sra/"):]
	j := strings.Index(rest, "/")
	if j < 0 {
		return "", errors.New("no trailing /<accession>.sra segment in url")
	}
	return rest[:j], nil
}

// tamperLastChar flips a character of an otherwise-valid base64url string to
// a different valid base64url character, guaranteed to change the decoded
// bytes.
//
// It flips index 0, not the last character, deliberately: RawURLEncoding
// (no padding) can leave the FINAL quantum's last character carrying a few
// bits the decoder simply discards (whenever the decoded length isn't a
// multiple of 3, the trailing quantum has "don't care" low bits) -- flipping
// into exactly those bits changes the encoded string without changing the
// decoded message at all, which was observed to flake this exact test.
// index 0 is always part of the FIRST quantum, which is always a full,
// meaningful 4-character group for any message longer than 3 bytes (true of
// every key this package mints), so flipping it always changes a real
// decoded byte.
func tamperLastChar(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	if s == "" {
		return "A"
	}
	orig := s[0]
	for i := 0; i < len(alphabet); i++ {
		if alphabet[i] != orig {
			return string(alphabet[i]) + s[1:]
		}
	}
	return s
}
