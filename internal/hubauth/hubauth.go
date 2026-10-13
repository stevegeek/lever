// Package hubauth signs the assertions lever's host side presents to the
// jail's scion hub: an ES256 key on the host, the hub holding only its public
// half, and one short-lived JWT per request. It imports nothing from lever.
package hubauth

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	AdminEmail      = "lever-admin@lever.local"
	ControllerEmail = "lever-controller@lever.local"
	Header          = "X-Lever-Hub-Assertion"
	Algorithm       = "ES256"
	Lifetime        = 90 * time.Second
	// Backdate absorbs guest clock drift between the reading and the request.
	Backdate = 30 * time.Second
)

// GuestPublicKeyPath is where the guest holds the public half for the hub:
// root-owned and outside every home, which scion's workstation routes let any
// loopback caller write under.
const GuestPublicKeyPath = "/etc/lever/hub-auth.pub"

const (
	adminSub      = "lever-admin"
	controllerSub = "lever-controller"
	remoteSub     = "lever-remote"
	devDomain     = "localhost"
)

// Issuer and Audience are mandatory: scion skips either check when the hub
// has none configured, so a token for another instance would pass.
func Issuer(instanceID string) string   { return "lever:" + instanceID }
func Audience(instanceID string) string { return "scion-hub:" + instanceID }

// IsReserved reports whether email names one of lever's own hub identities
// or scion's dev user (<os-user>@localhost). Both ToLower and EqualFold are
// used because they disagree on some runes (EqualFold folds U+017F to "s").
func IsReserved(email string) bool {
	e := strings.TrimSpace(email)
	at := strings.LastIndexByte(e, '@')
	if at >= 0 {
		domain := strings.TrimSuffix(e[at+1:], ".")
		if strings.ToLower(domain) == devDomain || strings.EqualFold(domain, devDomain) {
			return true
		}
		e = e[:at+1] + domain
	}
	for _, r := range []string{AdminEmail, ControllerEmail} {
		if strings.ToLower(e) == r || strings.EqualFold(e, r) {
			return true
		}
	}
	return false
}

// Clock is the guest's clock as an offset from the host's. The hub checks
// exp and iat against its own clock, so lever signs relative to it.
type Clock struct {
	Offset time.Duration
	now    func() time.Time
}

func (c Clock) Now() time.Time {
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	return now().Add(c.Offset)
}

// ClockFromGuestEpoch builds the offset from a guest `date +%s` reading
// taken at host time at.
func ClockFromGuestEpoch(guestEpoch int64, at time.Time) Clock {
	return Clock{Offset: time.Unix(guestEpoch, 0).Sub(at)}
}

type Signer struct {
	key        *Key
	instanceID string
	clock      Clock
}

func NewSigner(k *Key, instanceID string, c Clock) *Signer {
	return &Signer{key: k, instanceID: instanceID, clock: c}
}

type claims struct {
	Iss   string `json:"iss"`
	Aud   string `json:"aud"`
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Iat   int64  `json:"iat"`
	Exp   int64  `json:"exp"`
	Jti   string `json:"jti"`
}

var jwsHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","typ":"JWT"}`))

// Sign returns a compact ES256 JWS asserting email.
func (s *Signer) Sign(email, sub string) (string, error) {
	if s == nil || s.key == nil || s.key.priv == nil {
		return "", errors.New("hubauth: no signing key")
	}
	if err := checkToken("email", email); err != nil {
		return "", err
	}
	if local, domain, ok := strings.Cut(email, "@"); !ok || local == "" || domain == "" {
		return "", fmt.Errorf("hubauth: email %q is not an address", email)
	}
	if err := checkToken("sub", sub); err != nil {
		return "", err
	}
	if err := checkToken("instance id", s.instanceID); err != nil {
		return "", err
	}
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return "", fmt.Errorf("hubauth: jti: %w", err)
	}
	now := s.clock.Now()
	body, err := json.Marshal(claims{
		Iss:   Issuer(s.instanceID),
		Aud:   Audience(s.instanceID),
		Sub:   sub,
		Email: strings.ToLower(email),
		Iat:   now.Add(-Backdate).Unix(),
		Exp:   now.Add(Lifetime).Unix(),
		Jti:   base64.RawURLEncoding.EncodeToString(jti[:]),
	})
	if err != nil {
		return "", fmt.Errorf("hubauth: claims: %w", err)
	}
	input := jwsHeader + "." + base64.RawURLEncoding.EncodeToString(body)
	sum := sha256.Sum256([]byte(input))
	r, sg, err := ecdsa.Sign(rand.Reader, s.key.priv, sum[:])
	if err != nil {
		return "", fmt.Errorf("hubauth: sign: %w", err)
	}
	// JWS ES256 is the fixed-width r||s, not the ASN.1 form ecdsa emits.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	sg.FillBytes(sig[32:])
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// checkToken also refuses format characters (zero-width space, BOM): they
// make two addresses that print alike different hub users.
func checkToken(what, v string) error {
	if v == "" {
		return fmt.Errorf("hubauth: empty %s", what)
	}
	for _, r := range v {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return fmt.Errorf("hubauth: %s %q has whitespace, control or format characters", what, v)
		}
	}
	return nil
}

func (s *Signer) Admin() (string, error)      { return s.Sign(AdminEmail, adminSub) }
func (s *Signer) Controller() (string, error) { return s.Sign(ControllerEmail, controllerSub) }

// RemoteSigner is the remote proxy's signer. It is a positive allowlist so a
// login that slips past config validation still cannot become lever's own
// identity or scion's dev user.
type RemoteSigner struct {
	s       *Signer
	allowed map[string]bool
}

func NewRemoteSigner(s *Signer, allowedEmails []string) *RemoteSigner {
	allowed := map[string]bool{}
	for _, e := range allowedEmails {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" && !IsReserved(e) {
			allowed[e] = true
		}
	}
	return &RemoteSigner{s: s, allowed: allowed}
}

func (r *RemoteSigner) Sign(email string) (string, error) {
	if IsReserved(email) {
		return "", fmt.Errorf("hubauth: %q is reserved for lever's own hub identities or scion's dev user", email)
	}
	if !r.allowed[strings.ToLower(email)] {
		return "", fmt.Errorf("hubauth: %q is not an allowed remote login", email)
	}
	return r.s.Sign(email, remoteSub)
}
