package host

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stevegeek/lever/internal/config"
	"github.com/stevegeek/lever/internal/hubapi"
	"github.com/stevegeek/lever/internal/hubauth"
	"github.com/stevegeek/lever/internal/jail"
	"github.com/stevegeek/lever/internal/proc"
	"github.com/stevegeek/lever/internal/scion"
	"github.com/stevegeek/lever/internal/state"
)

func TestDoctorHubAuthWithoutAKeyFailsEveryRow(t *testing.T) {
	ctx := context.Background()
	st := state.State{Dir: t.TempDir()}
	jr := &hiJailRunner{t: t}
	app := hiRemoteApp(true, hiOperator("op@example.com"))
	for name, d := range map[string]doctorHubAuth{
		"no key":       newDoctorHubAuth(ctx, jr, st),
		"no transport": newDoctorHubAuth(ctx, nil, st),
	} {
		if d.err == nil {
			t.Fatalf("%s: no error", name)
		}
		for _, r := range []checkResult{checkHubAuthentication(ctx, d, nil), checkHubIdentities(ctx, d), checkRemoteWebRole(ctx, app, d)} {
			if r.ok || r.fix != hubAuthFix || !strings.Contains(r.detail, "cannot sign hub assertions") {
				t.Errorf("%s: %s = %+v", name, r.name, r)
			}
		}
	}
	// No logins and nothing recorded: there is nothing to check.
	if r := checkRemoteWebRole(ctx, hiRemoteApp(false), newDoctorHubAuth(ctx, jr, st)); !r.ok {
		t.Fatalf("remote web role with no logins = %+v", r)
	}
}

// hiDoctor is an instance with a key and an instance id whose jail answers
// AssertCurl's in-jail curl from the fake hub, after verifying the assertion
// the way the hub's JWT provider does.
type hiDoctor struct {
	*hiEnv
	key        *hubauth.Key
	acceptAny  bool
	devToken   bool
	providers  string
	assertions int
}

func newHIDoctor(t *testing.T, app *config.App) *hiDoctor {
	e := newHIEnv(t, app)
	key, _, err := hubauth.EnsureKey(e.st.HubAuthKey())
	if err != nil {
		t.Fatal(err)
	}
	id, err := e.st.EnsureInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	e.keyFP = key.Fingerprint()
	block, _ := pem.Decode(key.PublicPEM())
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub := parsed.(*ecdsa.PublicKey)
	d := &hiDoctor{hiEnv: e, key: key, providers: `{"providers":null,"authMode":"proxy"}`}
	e.jr.curl = func(env map[string]string, name string, args []string) (proc.Result, bool) {
		switch {
		case name == "date":
			return proc.Result{Stdout: strconv.FormatInt(e.now.Unix(), 10) + "\n"}, true
		case name == "sh" && len(args) == 5 && args[0] == "-c" && args[2] == "_" && env[jail.HubAssertionEnv] != "":
			if strings.Contains(strings.Join(args, " "), env[jail.HubAssertionEnv]) {
				t.Error("the assertion is on the jail command line")
			}
			d.assertions++
			path, ok := strings.CutPrefix(args[4], scion.DefaultHubEndpoint)
			if !ok {
				t.Errorf("AssertCurl reached %q, not the jail's hub", args[4])
			}
			var st int
			var b []byte
			if email, ok := d.verify(env[jail.HubAssertionEnv], pub, id); !ok {
				st, b = hiErr(401, "invalid assertion")
			} else {
				var body []byte
				if v, ok := env["LEVER_HUB_BODY"]; ok {
					body = []byte(v)
				}
				st, b = e.hub.handle(hiCaller{email: email}, args[3], path, body)
			}
			return proc.Result{Stdout: string(b) + "\n" + strconv.Itoa(st)}, true
		case name == "curl" && len(args) > 0 && strings.Contains(args[len(args)-1], "/auth/providers?clientType="):
			return proc.Result{Stdout: d.providers}, true
		case name == "sh" && len(args) == 5 && args[1] == hubAuthStateScript:
			out := string(key.PublicPEM())
			if d.devToken {
				out = "devtoken\n" + out
			}
			return proc.Result{Stdout: out}, true
		}
		return proc.Result{}, false
	}
	return d
}

// verify checks an ES256 compact JWS against the instance's public key, issuer,
// audience and the guest clock, and returns its email.
func (d *hiDoctor) verify(tok string, pub *ecdsa.PublicKey, instanceID string) (string, bool) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var c struct {
		Iss, Aud, Email string
		Iat, Exp        int64
	}
	if json.Unmarshal(payload, &c) != nil {
		return "", false
	}
	if d.acceptAny {
		return c.Email, true
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return "", false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return "", false
	}
	now := d.now.Unix()
	if c.Iss != hubauth.Issuer(instanceID) || c.Aud != hubauth.Audience(instanceID) || c.Exp <= now || c.Iat > now+5 {
		return "", false
	}
	return c.Email, true
}

func (d *hiDoctor) auth() doctorHubAuth {
	d.t.Helper()
	a := newDoctorHubAuth(context.Background(), d.jr, d.st)
	if a.err != nil {
		d.t.Fatal(a.err)
	}
	return a
}

func hiWantRow(t *testing.T, r checkResult, ok bool, subs ...string) {
	t.Helper()
	if r.ok != ok {
		t.Fatalf("%s: ok=%v, want %v: %s", r.name, r.ok, ok, r.detail)
	}
	for _, s := range subs {
		if !strings.Contains(r.detail, s) {
			t.Fatalf("%s: detail %q lacks %q", r.name, r.detail, s)
		}
	}
}

func TestDoctorHubAuthRowsOverTheJail(t *testing.T) {
	ctx := context.Background()
	op, contact := "op@example.com", "c1@example.com"
	d := newHIDoctor(t, hiRemoteApp(true, hiOperator(op), hiContact(contact)))
	d.mustRun()

	hiWantRow(t, checkHubAuthentication(ctx, d.auth(), nil), true, d.key.Fingerprint())
	hiWantRow(t, checkHubIdentities(ctx, d.auth()), true, "fresh mode")
	hiWantRow(t, checkRemoteWebRole(ctx, d.app, d.auth()), true, op, contact)
	if d.assertions == 0 {
		t.Fatal("no request went through AssertCurl")
	}

	t.Run("hub accepts any key", func(t *testing.T) {
		d.acceptAny = true
		defer func() { d.acceptAny = false }()
		hiWantRow(t, checkHubAuthentication(ctx, d.auth(), nil), false, "another key, want 401", "another hub (iss/aud), want 401")
	})
	t.Run("dev token and a login provider", func(t *testing.T) {
		d.devToken, d.providers = true, `{"providers":[{"id":"github"}]}`
		defer func() { d.devToken, d.providers = false, `{"providers":null,"authMode":"proxy"}` }()
		hiWantRow(t, checkHubAuthentication(ctx, d.auth(), nil), false, "dev token", "login provider")
	})
	t.Run("extra admin, unrecorded token, dev user", func(t *testing.T) {
		h := d.hub
		x := h.addUser("", "x@example.com", "admin", "active", d.now)
		ctl := h.byEmail(hubauth.ControllerEmail)
		h.tokens = append(h.tokens, &hiToken{ID: "tok-extra", Owner: ctl.ID, Name: "sneaky", Secret: "scion_pat_extra"})
		h.addUser(devUserID, "scion@localhost", "member", "active", d.now)
		defer func() {
			h.removeUser(x.ID)
			h.removeUser(devUserID)
			h.tokens = slices.DeleteFunc(h.tokens, func(t *hiToken) bool { return t.ID == "tok-extra" })
		}()
		hiWantRow(t, checkHubIdentities(ctx, d.auth()), false, x.ID, "tok-extra (sneaky)", "dev user exists")
	})
	t.Run("unbound and unsuspended logins", func(t *testing.T) {
		h := d.hub
		c := h.byEmail(contact)
		kept := slices.Clone(h.bindings)
		h.bindings = slices.DeleteFunc(h.bindings, func(b hubapi.RoleBinding) bool { return b.PrincipalID == c.ID })
		app := hiRemoteApp(true, hiOperator(op), hiOperator("new@example.com"))
		defer func() { h.bindings = kept }()
		hiWantRow(t, checkRemoteWebRole(ctx, app, d.auth()), false,
			"new@example.com has no hub user", contact+" was removed from the config but its hub user is invited")
		hiWantRow(t, checkRemoteWebRole(ctx, d.app, d.auth()), false, contact+" is not bound to "+contactRoleName)
	})
	t.Run("record names another key", func(t *testing.T) {
		rec := d.record()
		rec.KeyFingerprint = "sha256:other"
		if err := d.st.SaveHubIdentity(rec); err != nil {
			t.Fatal(err)
		}
		hiWantRow(t, checkHubIdentities(ctx, d.auth()), false, "sha256:other")
	})
}

func TestDoctorHubIdentitiesOnAMigratedHub(t *testing.T) {
	ctx := context.Background()
	d := newHIDoctor(t, hiRemoteApp(false))
	d.seedMigrated("scion@localhost", false)
	// The controller's tokens are the recorded PAT only.
	d.mustRun()
	hiWantRow(t, checkHubIdentities(ctx, d.auth()), true, "migrated mode")

	// An agent renamed the controller since the last apply.
	d.hub.user(devUserID).Email = "someone@example.com"
	hiWantRow(t, checkHubIdentities(ctx, d.auth()), false, `"someone@example.com" role "member"`)
}
