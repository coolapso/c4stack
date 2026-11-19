//go:build integration

package local

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"
)

// authenticator is a software passkey authenticator: it creates discoverable ES256
// credentials with "none" attestation, and signs assertions, as a browser's would.
type authenticator struct {
	t      *testing.T
	rpID   string
	origin string
	creds  []*softCredential
	// counter makes credentials keep a signature counter; synced passkeys don't.
	counter bool
	// backup marks credentials as backup eligible and backed up (synced).
	backup bool
	// transports is what the browser reports; nil leaves the field out.
	transports []string
}

type softCredential struct {
	id         []byte
	key        *ecdsa.PrivateKey
	userHandle []byte
	count      uint32
}

func newAuthenticator(t *testing.T) *authenticator {
	return &authenticator{t: t, rpID: "localhost", origin: publicURL, transports: []string{"internal"}}
}

var b64 = base64.RawURLEncoding

// create answers a registration's options.
func (a *authenticator) create(options []byte) map[string]any {
	a.t.Helper()
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			User      struct {
				ID string `json:"id"`
			} `json:"user"`
			Exclude []struct {
				ID string `json:"id"`
			} `json:"excludeCredentials"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &o); err != nil {
		a.t.Fatal(err)
	}
	handle, err := b64.DecodeString(o.PublicKey.User.ID)
	if err != nil {
		a.t.Fatal(err)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	cred := &softCredential{id: make([]byte, 32), key: key, userHandle: handle}
	_, _ = rand.Read(cred.id)
	a.creds = append(a.creds, cred)

	clientData := a.clientData("webauthn.create", o.PublicKey.Challenge)
	pub, _ := key.PublicKey.Bytes() // 0x04 || X || Y
	cose := cborMap(
		cborInt(1), cborInt(2), // kty: EC2
		cborInt(3), cborInt(-7), // alg: ES256
		cborInt(-1), cborInt(1), // crv: P-256
		cborInt(-2), cborBytes(pub[1:33]),
		cborInt(-3), cborBytes(pub[33:]),
	)
	authData := a.authData(0x41, 0) // UP, AT
	authData = append(authData, make([]byte, 16)...)
	authData = binary.BigEndian.AppendUint16(authData, uint16(len(cred.id)))
	authData = append(authData, cred.id...)
	authData = append(authData, cose...)
	attestation := cborMap(
		cborText("fmt"), cborText("none"),
		cborText("attStmt"), cborMap(),
		cborText("authData"), cborBytes(authData),
	)
	response := map[string]any{
		"clientDataJSON":    b64.EncodeToString(clientData),
		"attestationObject": b64.EncodeToString(attestation),
	}
	if a.transports != nil {
		response["transports"] = a.transports
	}
	return map[string]any{
		"id":                     b64.EncodeToString(cred.id),
		"rawId":                  b64.EncodeToString(cred.id),
		"type":                   "public-key",
		"response":               response,
		"clientExtensionResults": map[string]any{},
	}
}

// get answers an assertion's options with a credential.
func (a *authenticator) get(options []byte, cred *softCredential) map[string]any {
	a.t.Helper()
	var o struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
		} `json:"publicKey"`
	}
	if err := json.Unmarshal(options, &o); err != nil {
		a.t.Fatal(err)
	}
	return a.sign(o.PublicKey.Challenge, cred)
}

func (a *authenticator) sign(challenge string, cred *softCredential) map[string]any {
	a.t.Helper()
	if a.counter {
		cred.count++
	}
	clientData := a.clientData("webauthn.get", challenge)
	authData := a.authData(0x01, cred.count) // UP
	sum := sha256.Sum256(clientData)
	sig, err := ecdsa.SignASN1(rand.Reader, cred.key, sha256Sum(append(append([]byte{}, authData...), sum[:]...)))
	if err != nil {
		a.t.Fatal(err)
	}
	return map[string]any{
		"id":    b64.EncodeToString(cred.id),
		"rawId": b64.EncodeToString(cred.id),
		"type":  "public-key",
		"response": map[string]any{
			"clientDataJSON":    b64.EncodeToString(clientData),
			"authenticatorData": b64.EncodeToString(authData),
			"signature":         b64.EncodeToString(sig),
			"userHandle":        b64.EncodeToString(cred.userHandle),
		},
		"clientExtensionResults": map[string]any{},
	}
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func (a *authenticator) clientData(typ, challenge string) []byte {
	data, _ := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": a.origin, "crossOrigin": false})
	return data
}

// authData is the authenticator data's start: the RP ID's hash, flags with UV set, and
// the counter.
func (a *authenticator) authData(flags byte, count uint32) []byte {
	rp := sha256.Sum256([]byte(a.rpID))
	flags |= 0x04 // UV
	if a.backup {
		flags |= 0x08 | 0x10 // BE, BS
	}
	out := append(rp[:], flags)
	return binary.BigEndian.AppendUint32(out, count)
}

// A minimal CBOR encoder, enough for COSE keys and attestation objects.

func cborHead(major byte, n uint64) []byte {
	switch {
	case n < 24:
		return []byte{major<<5 | byte(n)}
	case n < 1<<8:
		return []byte{major<<5 | 24, byte(n)}
	case n < 1<<16:
		return binary.BigEndian.AppendUint16([]byte{major<<5 | 25}, uint16(n))
	default:
		return binary.BigEndian.AppendUint32([]byte{major<<5 | 26}, uint32(n))
	}
}

func cborInt(n int) []byte {
	if n >= 0 {
		return cborHead(0, uint64(n))
	}
	return cborHead(1, uint64(-1-n))
}

func cborBytes(b []byte) []byte { return append(cborHead(2, uint64(len(b))), b...) }
func cborText(s string) []byte  { return append(cborHead(3, uint64(len(s))), s...) }

func cborMap(items ...[]byte) []byte {
	out := cborHead(5, uint64(len(items)/2))
	for _, item := range items {
		out = append(out, item...)
	}
	return out
}
