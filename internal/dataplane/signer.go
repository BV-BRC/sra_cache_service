// Package dataplane implements the SRACache data plane: the plain-HTTP
// listener that serves cached .sra bytes via a short-lived signed URL, and
// the signer that mints/verifies those URLs.
//
// Modeled on Workspace's get_download_url / ws-download
// (Workspace/go/internal/dlservice) as the closest existing precedent for a
// signed-URL data plane in this codebase -- but deliberately NOT copying
// its mechanism. Workspace mints an opaque, random download key and stores
// it (plus the object it names) as a row in Mongo; verifying a request means
// looking that row up. SRACache has no such store to put a signed-URL record
// in (the sqlite catalog holds accession state, not per-URL grants), and
// PLAN-sra-precache-service.md section 3 deliberately chose to reuse the
// URL's own `expires` as the cache-eviction pin lease rather than add a
// second protocol -- so the URL here is self-contained: an HMAC over the
// accession and its expiry, verifiable with no lookup at all. See
// PLAN-sra-cache-service-implementation.md, Phase 7.
package dataplane

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrEmptySecret is returned by Mint and Verify when the Signer was built
// with an empty secret. Per the implementation plan, a guessable (or
// missing) signing key would make the whole cache publicly readable, so
// this package refuses outright rather than operating with a weak key.
// Refusing to *start the whole service* over this is Phase 10's job; this
// phase's job is to fail loudly and specifically the moment anyone actually
// tries to mint or verify a URL.
var ErrEmptySecret = errors.New("dataplane: signer has an empty secret; refusing to mint or verify a URL")

// ErrExpired is returned by Verify for a syntactically well-formed,
// correctly-signed key whose expiry has already passed.
var ErrExpired = errors.New("dataplane: signed URL has expired")

// ErrInvalidSignature is returned by Verify for a key that is malformed, or
// whose signature does not match -- including a key minted for a different
// accession, or one where the accession, expiry or nonce has been tampered
// with. Deliberately not more specific than this (e.g. "wrong accession"
// vs. "bad hex") -- distinguishing those would let a caller probe the
// signing scheme by observing which failure mode a crafted key produces.
var ErrInvalidSignature = errors.New("dataplane: invalid signature")

// nonceSize is the number of random bytes mixed into every minted key. Its
// only job is to make two Mint calls for the same accession and the same
// rounded expiry second produce different keys (harmless on its own, but
// avoids a stable, replayable URL for the same accession/expiry pair ever
// recurring), not to add secrecy -- the HMAC is what actually protects the
// key.
const nonceSize = 12

// Signer mints and verifies signed data-plane URLs of the form
// "<base>/sra/<key>/<accession>.sra".
//
// # Key encoding
//
// <key> is the URL-safe base64 (no padding) encoding of the ASCII string
//
//	<expires_unix>:<nonce_hex>:<hmac_hex>
//
// where:
//   - expires_unix is expiry as a base-10 Unix timestamp (seconds).
//   - nonce_hex is nonceSize random bytes, hex-encoded.
//   - hmac_hex is hex(HMAC-SHA256(secret, message)), and message is built
//     as described below.
//
// Colons cannot appear in expires_unix (digits only) or in hex-encoded
// nonce/hmac, so splitting the decoded key on ":" is unambiguous -- no
// separate escaping is needed for this layer. Base64-encoding the whole
// thing (rather than shipping the colon-joined string directly in the URL
// path) sidesteps having to think about which of ":" is even reencoded in a
// URL path segment.
//
// # HMAC message encoding
//
// The plan calls for "HMAC-SHA256 over accession | expires | nonce" and
// requires that the delimiter scheme be unambiguous -- no accession/expiry
// string can be crafted to collide across the "|" the way, say,
// "AB"+"C" and "A"+"BC" would collide under naive concatenation. This
// implementation length-prefixes each field instead of joining them with a
// literal separator byte, which sidesteps the collision question entirely
// (there is no delimiter byte for a crafted field to smuggle in). The
// message fed to HMAC is:
//
//	uint32be(len(accession)) || accession || uint64be(expiresUnix) || nonce
//
// nonce is fixed-size (nonceSize bytes), so it needs no length prefix of its
// own; accession is caller-controlled and variable-length, so it gets one.
// Verify must reconstruct this exact byte sequence from the accession named
// in the URL path plus the expiry/nonce decoded from the key -- both sides
// live in this file so they can never drift apart.
type Signer struct {
	secret  []byte
	baseURL string
}

// New builds a Signer from secret (typically config.Config.DownloadSecret)
// and baseURL (config.Config.DataPlaneBaseURL, the externally visible
// data-plane base URL -- see that field's doc comment for why it is not
// inferred from a listen address). An empty secret is accepted here without
// error -- Load-time config validation (Phase 10's job) is what should
// refuse to start the service on an empty secret; this package instead
// fails every individual Mint/Verify call clearly and specifically
// (ErrEmptySecret), so a misconfigured service is loud on first real use
// rather than silently signing with a zero-length key. An empty baseURL is
// likewise accepted here and instead fails Mint (there is no URL to build).
func New(secret, baseURL string) *Signer {
	return &Signer{secret: []byte(secret), baseURL: baseURL}
}

// buildMessage constructs the exact byte sequence Mint and Verify both HMAC
// -- see Signer's doc comment for the encoding and why it's collision-safe.
func buildMessage(accession string, expiresUnix int64, nonce []byte) []byte {
	msg := make([]byte, 0, 4+len(accession)+8+len(nonce))
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(accession)))
	msg = append(msg, lenBuf[:]...)
	msg = append(msg, accession...)
	var expBuf [8]byte
	binary.BigEndian.PutUint64(expBuf[:], uint64(expiresUnix))
	msg = append(msg, expBuf[:]...)
	msg = append(msg, nonce...)
	return msg
}

func (s *Signer) hmacFor(accession string, expiresUnix int64, nonce []byte) []byte {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write(buildMessage(accession, expiresUnix, nonce))
	return mac.Sum(nil)
}

// Mint returns a signed data-plane URL for accession, valid for ttl from
// now, and the absolute expiry time it embeds. Fails (ErrEmptySecret) if
// the Signer's secret is empty, or if baseURL was empty at construction
// (there is no base to build a URL against).
//
// The returned expires is also, per PLAN-sra-precache-service.md section 3,
// the cache-eviction pin lease for accession: the caller is expected to
// pass it to catalog.Catalog.Pin in the same request (see
// internal/rpcapi/data.go), not just embed it in the URL.
func (s *Signer) Mint(accession string, ttl time.Duration) (signedURL string, expires time.Time, err error) {
	if len(s.secret) == 0 {
		return "", time.Time{}, ErrEmptySecret
	}
	if s.baseURL == "" {
		return "", time.Time{}, fmt.Errorf("dataplane: mint: no data-plane base URL configured")
	}
	if accession == "" {
		return "", time.Time{}, fmt.Errorf("dataplane: mint: accession is empty")
	}

	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", time.Time{}, fmt.Errorf("dataplane: mint: generating nonce: %w", err)
	}

	expires = time.Now().Add(ttl).UTC()
	expiresUnix := expires.Unix()
	sum := s.hmacFor(accession, expiresUnix, nonce)

	raw := fmt.Sprintf("%d:%s:%s", expiresUnix, hex.EncodeToString(nonce), hex.EncodeToString(sum))
	key := base64.RawURLEncoding.EncodeToString([]byte(raw))

	signedURL = strings.TrimRight(s.baseURL, "/") + "/sra/" + key + "/" + url.PathEscape(accession) + ".sra"
	return signedURL, expires, nil
}

// Verify checks that key is a well-formed, correctly-signed, unexpired
// signature for accession as of now.
//
// Returns nil on success; ErrEmptySecret, ErrExpired, or ErrInvalidSignature
// otherwise -- callers (the HTTP handler in dataplane.go) use these to pick
// a status code and log message, even though the plan calls for all of them
// to ultimately reach the client as a 4xx.
func (s *Signer) Verify(accession, key string) error {
	return s.verifyAt(accession, key, time.Now())
}

// verifyAt is Verify with the current time passed in explicitly, so tests
// can exercise expiry deterministically without sleeping.
func (s *Signer) verifyAt(accession, key string, now time.Time) error {
	if len(s.secret) == 0 {
		return ErrEmptySecret
	}

	decoded, err := base64.RawURLEncoding.DecodeString(key)
	if err != nil {
		return fmt.Errorf("%w: not valid base64url: %v", ErrInvalidSignature, err)
	}

	parts := strings.SplitN(string(decoded), ":", 3)
	if len(parts) != 3 {
		return fmt.Errorf("%w: malformed key", ErrInvalidSignature)
	}
	expiresUnix, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return fmt.Errorf("%w: malformed expiry", ErrInvalidSignature)
	}
	nonce, err := hex.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("%w: malformed nonce", ErrInvalidSignature)
	}
	wantSum, err := hex.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("%w: malformed signature", ErrInvalidSignature)
	}

	gotSum := s.hmacFor(accession, expiresUnix, nonce)
	// Constant-time comparison: a timing side-channel on this check would
	// let an attacker recover a valid signature byte by byte.
	if !hmac.Equal(gotSum, wantSum) {
		return ErrInvalidSignature
	}

	// Signature checked out; expiry is checked last and separately so a
	// caller gets a distinct, honest "expired" error rather than "invalid
	// signature" for a key that WAS validly issued and has simply aged out
	// -- useful for the HTTP handler and for anyone debugging a report of a
	// download failing (a curl retried an hour later looks very different
	// operationally from a tampered link).
	if !now.Before(time.Unix(expiresUnix, 0).UTC()) {
		return ErrExpired
	}
	return nil
}
