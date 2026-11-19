package local

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"net/mail"
	"regexp"
	"strings"
	"unicode"
)

// normalizeAddress trims and lowercases an address, and reports whether it is one plain
// address, without a display name, of at most maxAddressLength bytes. The lowercase
// address is stored and compared everywhere.
func normalizeAddress(raw string) (string, bool) {
	a := strings.ToLower(strings.TrimSpace(raw))
	if a == "" || len(a) > maxAddressLength {
		return "", false
	}
	for _, r := range a {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", false
		}
	}
	parsed, err := mail.ParseAddress(a)
	if err != nil || parsed.Name != "" || parsed.Address != a {
		return "", false
	}
	at := strings.LastIndexByte(a, '@')
	if at < 1 || at == len(a)-1 || strings.HasPrefix(a, `"`) || strings.HasSuffix(a, "]") {
		return "", false
	}
	return a, true
}

// newCode returns codeDigits random digits.
func newCode() string {
	limit := big.NewInt(1)
	for range codeDigits {
		limit.Mul(limit, big.NewInt(10))
	}
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		panic(err) // crypto/rand never fails
	}
	s := n.String()
	return strings.Repeat("0", codeDigits-len(s)) + s
}

// formatCode writes a code in two groups, as the emails show it: "4821 0937".
func formatCode(code string) string {
	half := len(code) / 2
	return code[:half] + " " + code[half:]
}

var nonDigits = regexp.MustCompile(`[\s-]`)

// parseCode takes a code as people type or paste it, with or without the space.
func parseCode(raw string) (string, bool) {
	code := nonDigits.ReplaceAllString(raw, "")
	if len(code) != codeDigits {
		return "", false
	}
	for _, r := range code {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return code, true
}

// codeHash is the HMAC a code is stored as. The code's ID is part of it, so equal codes
// have different hashes, and a database copy can't be used to guess codes offline.
func (l *Local) codeHash(codeID, code string) []byte {
	mac := hmac.New(sha256.New, l.codeKey)
	mac.Write([]byte(codeID))
	mac.Write([]byte{0})
	mac.Write([]byte(code))
	return mac.Sum(nil)
}

// newToken returns a session token: 32 random bytes.
func newToken() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}

func encodeToken(token []byte) string {
	return base64.RawURLEncoding.EncodeToString(token)
}

func decodeToken(s string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil
	}
	return b
}

// hashToken is how a session token is stored. Tokens have 256 bits, so a plain hash is
// enough, and rotating the secret key leaves sessions alone.
func hashToken(token []byte) []byte {
	sum := sha256.Sum256(token)
	return sum[:]
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return uuidString(b)
}

func uuidString(b []byte) string {
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// uuidBytes returns a UUID's 16 bytes: the user handle passkeys carry.
func uuidBytes(id string) []byte {
	b, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil || len(b) != 16 {
		return nil
	}
	return b
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validUUID(id string) bool {
	return uuidPattern.MatchString(id)
}

// newDeviceID returns the random ID of a browser that signed in, which names its own
// counters.
func newDeviceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
