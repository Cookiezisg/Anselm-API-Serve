// Package pow implements the stateless proof-of-work challenge for /v1/install.
//
// 无状态自验:challenge = base64url(rand16 ‖ unixTs(8) ‖ HMAC-SHA256(secret,
// rand16‖ts)[:16])。验证纯 CPU(重算 HMAC + 查时戳),不建表、不落 DB——隐私上
// challenge 只含随机数 + 时戳,绝无用户数据。
//
// This package is provider-agnostic infra: it returns plain sentinel errors and
// the caller (app/install) maps them to wire codes. It MUST NOT import
// domain/apierr — the layering rule forbids pkg -> domain.
package pow

import (
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"time"
)

// ChallengeTTL is the freshness window for an issued challenge: outside |now-issued|
// the challenge is rejected (client must refetch). It doubles as the nonce-once
// cache TTL — past it the timestamp check rejects the challenge regardless, so a
// consumed entry is safe to evict.
//
// 120s 兼顾客户端求解 + 提交往返,又够短限制重放窗口。
const ChallengeTTL = 120 * time.Second

// Field widths of the decoded challenge: 16 random bytes (anti-guessing) ‖ 8-byte
// big-endian unix seconds ‖ 16-byte truncated HMAC tag. 40 bytes -> ~54 base64url
// chars.
const (
	randLen = 16
	tsLen   = 8
	macLen  = 16
	rawLen  = randLen + tsLen + macLen
)

// Difficulty bounds: a challenge demands [MinDifficulty, MaxDifficulty] leading
// zero bits. Above 32 the expected solve cost explodes past any client budget and
// would also overflow reasonable bit accounting; below 1 there is no work at all.
const (
	MinDifficulty = 1
	MaxDifficulty = 32
)

// Sentinel errors. The caller maps these to wire codes; pkg never imports apierr.
var (
	// ErrBadDifficulty is returned by Mint/Verify when difficulty is outside
	// [MinDifficulty, MaxDifficulty].
	ErrBadDifficulty = errors.New("pow: difficulty out of range")
	// ErrMalformed is a structurally invalid header/challenge (bad base64, wrong
	// length, missing separator). Treated as a forgery class by callers.
	ErrMalformed = errors.New("pow: malformed challenge")
	// ErrForged means the HMAC tag did not authenticate the challenge.
	ErrForged = errors.New("pow: forged challenge")
	// ErrExpired means the embedded timestamp is outside the freshness window
	// (past OR future).
	ErrExpired = errors.New("pow: expired challenge")
	// ErrInsufficient means SHA256(challenge"."nonce) has fewer than `difficulty`
	// leading zero bits.
	ErrInsufficient = errors.New("pow: insufficient difficulty")
	// ErrReplayed means the challenge was already consumed (nonce-once).
	ErrReplayed = errors.New("pow: challenge replayed")
)

// NonceUser is the single-use replay guard the caller injects (satisfied by
// noncecache.Cache). Keeping it an interface keeps pow free of the noncecache
// import and lets tests inject a stub. UseOnce returns true on the first call for
// a challenge and false on any replay within its TTL.
type NonceUser interface {
	UseOnce(challenge string) bool
}

// Mint produces a fresh stateless challenge signed by secret. random16 makes each
// challenge unique + unguessable; the embedded unix timestamp enables stateless
// expiry; the truncated HMAC binds (random‖ts) so a client cannot forge a
// challenge with an arbitrary (newer) timestamp. Returns the base64url string the
// client echoes back. difficulty is validated so a caller cannot mint a challenge
// it could never verify.
func Mint(secret []byte, difficulty int, now time.Time) (string, error) {
	if difficulty < MinDifficulty || difficulty > MaxDifficulty {
		return "", ErrBadDifficulty
	}
	buf := make([]byte, rawLen)
	if _, err := cryptorand.Read(buf[:randLen]); err != nil {
		return "", err
	}
	// now.Unix() is seconds since epoch; for any realistic clock this is a small
	// positive value far below int64/uint64 bounds, so the conversion is exact.
	putUnix(buf[randLen:randLen+tsLen], now.Unix())
	tag := mac(secret, buf[:randLen+tsLen])
	copy(buf[randLen+tsLen:], tag[:macLen])
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Verify checks a (challenge, nonce) pair in the spec-mandated order, cheapest
// forgery defense first and the single-use mark LAST so a request failing an
// earlier check never burns the challenge:
//
//	(1) HMAC authenticity (constant-time)  -> ErrForged
//	(2) freshness (|now-issued| <= TTL)    -> ErrExpired
//	(3) difficulty (leading zero bits)     -> ErrInsufficient
//	(4) nonce-once (nonces.UseOnce)        -> ErrReplayed
//
// nil error means the solution is valid AND the challenge has now been consumed.
func Verify(secret []byte, challenge, nonce string, difficulty int, now time.Time, nonces NonceUser) error {
	if difficulty < MinDifficulty || difficulty > MaxDifficulty {
		return ErrBadDifficulty
	}

	raw, err := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil || len(raw) != rawLen {
		return ErrMalformed
	}

	// (1) HMAC authenticity — constant-time compare of the truncated tag to avoid
	// a timing oracle on the HMAC prefix.
	signed := raw[:randLen+tsLen]
	gotTag := raw[randLen+tsLen:]
	wantTag := mac(secret, signed)
	if subtle.ConstantTimeCompare(gotTag, wantTag[:macLen]) != 1 {
		return ErrForged
	}

	// (2) freshness. The big-endian field was written from a non-negative
	// now.Unix(); read it back through a width-checked helper. Reject both a past
	// and a (skew/forged) future timestamp beyond the slack.
	issued := time.Unix(readUnix(raw[randLen:randLen+tsLen]), 0)
	if now.Sub(issued) > ChallengeTTL || issued.Sub(now) > ChallengeTTL {
		return ErrExpired
	}

	// (3) difficulty.
	if !meetsDifficulty(challenge, nonce, difficulty) {
		return ErrInsufficient
	}

	// (4) nonce-once LAST: burn the challenge only on an otherwise-valid solve.
	if !nonces.UseOnce(challenge) {
		return ErrReplayed
	}
	return nil
}

// ParseHeader splits the "X-PoW: <challenge>.<nonce>" header on its FIRST '.'.
// The challenge is base64url (no dot), so the first segment is the challenge and
// the entire remainder is the nonce. ok=false when empty or missing/edge-only
// separator.
//
// 点分;challenge 不含点,故首段为 challenge、其余为 nonce。
func ParseHeader(h string) (challenge, nonce string, ok bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", "", false
	}
	i := strings.IndexByte(h, '.')
	if i <= 0 || i == len(h)-1 {
		return "", "", false
	}
	return h[:i], h[i+1:], true
}

// mac computes the full HMAC-SHA256 tag over msg with secret.
func mac(secret, msg []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(msg)
	return m.Sum(nil)
}

// meetsDifficulty reports whether SHA256(challenge "." nonce) has at least
// `difficulty` leading zero bits. The hash input mirrors exactly what the client
// hashes, so the algorithm is self-consistent end to end.
func meetsDifficulty(challenge, nonce string, difficulty int) bool {
	h := sha256.Sum256([]byte(challenge + "." + nonce))
	return leadingZeroBits(h[:]) >= difficulty
}

// leadingZeroBits counts leading zero bits in b (a SHA-256 digest).
func leadingZeroBits(b []byte) int {
	n := 0
	for _, by := range b {
		if by == 0 {
			n += 8
			continue
		}
		for mask := byte(0x80); mask != 0; mask >>= 1 {
			if by&mask != 0 {
				return n
			}
			n++
		}
		return n
	}
	return n
}

// putUnix / readUnix marshal a unix-seconds timestamp as 8 big-endian bytes.
// encoding/binary works in uint64; a unix-seconds value is always well within
// int64 and (being a date, not arbitrary input) non-negative, so the int64<->
// uint64 round-trip is bit-exact. Centralizing the conversions here keeps the
// gosec G115 reasoning in one audited spot rather than scattered casts.
func putUnix(dst []byte, sec int64) {
	binary.BigEndian.PutUint64(dst, uint64(sec)) //nolint:gosec // G115: unix-seconds is non-negative & << 2^63; round-trips bit-exact, see readUnix
}

func readUnix(src []byte) int64 {
	return int64(binary.BigEndian.Uint64(src)) //nolint:gosec // G115: value was written by putUnix from a valid time; round-trips bit-exact
}
