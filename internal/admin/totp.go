package admin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"net/url"
	"strings"
	"time"
)

// totpStep is the RFC 6238 time step in seconds.
const totpStep = 30

var totpBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateTOTPSecret returns a new random 160-bit base32 secret.
func GenerateTOTPSecret() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return totpBase32.EncodeToString(buf), nil
}

// TOTPCode computes the 6-digit RFC 6238 (HMAC-SHA1, 30s step) code for a
// base32 secret at time t.
func TOTPCode(secret string, t time.Time) string {
	key, err := totpBase32.DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil || len(key) == 0 || t.Unix() < 0 {
		return ""
	}
	counter := uint64(t.Unix()) / totpStep
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	binaryCode := (uint32(sum[offset])&0x7f)<<24 |
		(uint32(sum[offset+1])&0xff)<<16 |
		(uint32(sum[offset+2])&0xff)<<8 |
		(uint32(sum[offset+3]) & 0xff)
	code := binaryCode % 1000000
	return string('0'+byte(code/100000%10)) + string('0'+byte(code/10000%10)) +
		string('0'+byte(code/1000%10)) + string('0'+byte(code/100%10)) +
		string('0'+byte(code/10%10)) + string('0'+byte(code%10))
}

// VerifyTOTP checks a user-entered code against the current and adjacent time
// steps (±1 window for clock skew). The comparison is constant-time.
func VerifyTOTP(secret, code string) bool {
	_, ok := verifyTOTPCounter(secret, code, time.Now(), -1)
	return ok
}

func verifyTOTPCounter(secret, code string, now time.Time, last int64) (int64, bool) {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return 0, false
	}
	for _, c := range code {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	for _, step := range []int64{0, -1, 1} {
		counter := now.Unix()/totpStep + step
		if counter <= last {
			continue
		}
		want := TOTPCode(secret, now.Add(time.Duration(step)*totpStep*time.Second))
		if subtle.ConstantTimeCompare([]byte(code), []byte(want)) == 1 {
			return counter, true
		}
	}
	return 0, false
}

// TOTPAuthURL builds an otpauth:// URL for authenticator apps.
func TOTPAuthURL(secret, username string) string {
	if username == "" {
		username = "admin"
	}
	return "otpauth://totp/" + url.PathEscape("aisense:"+username) +
		"?secret=" + url.QueryEscape(secret) + "&issuer=aisense&digits=6&period=30"
}
