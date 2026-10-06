package webpush

import (
	"errors"
	"strings"
	"testing"
)

const goodP256 = vecUAPublic
const goodAuth = vecAuth

func TestCheckEndpointAllowList(t *testing.T) {
	for _, ok := range []string{
		"https://fcm.googleapis.com/fcm/send/abc:APA91b",
		"https://web.push.apple.com/QGy2wDm9",
		"https://updates.push.services.mozilla.com/wpush/v2/gAAAA",
		"https://wns2-par02p.notify.windows.com/w/?token=BQYAAA",
	} {
		if _, err := CheckEndpoint(ok, nil); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://fcm.googleapis.com/fcm/send/x",                    // http
		"https://fcm.googleapis.com:8443/x",                       // port
		"https://fcm.googleapis.com:443/wp/abc",                   // explicit default port: a second spelling
		"https://fcm.googleapis.com:/wp/abc",                      // empty port
		"HTTPS://fcm.googleapis.com/wp/abc",                       // scheme case
		"Https://fcm.googleapis.com/wp/abc",                       // scheme case
		"https://fcm.googleapis.com/wp/%61bc",                     // escaped letter: prints as /wp/abc
		"https://fcm.googleapis.com/wp/abc?",                      // empty query: prints without it
		"https://user@fcm.googleapis.com/x",                       // userinfo
		"https://fcm.googleapis.com.evil.test/x",                  // suffix trick
		"https://evilfcm.googleapis.com/x",                        // not exact
		"https://push.apple.com/x",                                // bare suffix
		"https://.push.apple.com/x",                               // empty label
		"https://-a.push.apple.com/x",                             // label rule
		"https://a_b.notify.windows.com/x",                        // label rule
		"https://FCM.googleapis.com/x",                            // case
		"https://fcm.googleapis.com./x",                           // trailing dot
		"https://142.250.180.10/x",                                // IP literal
		"https://[2a00:1450::1]/x",                                // IPv6 literal
		"https://127.0.0.1:9447/push/op",                          // loopback, no test hosts
		"https://fcm.googleapis.com/x#frag",                       // fragment
		"https://fcm.googleapis.com",                              // no path
		"https://fcm.googleapis.com/a b",                          // space
		"https://fcm.googleapis.com/" + strings.Repeat("a", 1100), // length
		"mailto:x@fcm.googleapis.com",
		"",
	} {
		if _, err := CheckEndpoint(bad, nil); !errors.Is(err, ErrEndpoint) {
			t.Errorf("%q accepted (%v)", bad, err)
		}
	}
}

func TestTestHostsAdmitOnlyTheirLoopbackAddress(t *testing.T) {
	th, err := ParseTestHosts("127.0.0.1:9447")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CheckEndpoint("http://127.0.0.1:9447/push/op", th); err != nil {
		t.Fatalf("test host refused: %v", err)
	}
	for _, bad := range []string{"http://127.0.0.1:9448/push/op", "http://localhost:9447/x", "https://127.0.0.1:9447/x"} {
		if _, err := CheckEndpoint(bad, th); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	for _, v := range []string{"localhost:9447", "10.0.0.1:443", "[::1]:9447", "127.0.0.1", "127.0.0.1:0", "127.0.0.1:9447,example.com:443", " 127.0.0.01:9447"} {
		if _, err := ParseTestHosts(v); err == nil {
			t.Errorf("ParseTestHosts(%q) accepted", v)
		}
	}
	if th, err := ParseTestHosts(""); err != nil || len(th) != 0 {
		t.Fatalf("empty: %v %v", th, err)
	}
}

func TestParseSubscriptionKeys(t *testing.T) {
	ep := "https://fcm.googleapis.com/fcm/send/abc"
	s, err := ParseSubscription(ep, goodP256+"=", goodAuth+"==", nil)
	if err != nil || s.P256DH != goodP256 || s.Auth != goodAuth || s.Endpoint != ep {
		t.Fatalf("%+v %v", s, err)
	}
	for name, k := range map[string][2]string{
		"short p256dh": {goodP256[:40], goodAuth},
		"not on curve": {"BA" + strings.Repeat("A", 85), goodAuth},
		"short auth":   {goodP256, "AAAA"},
		"bad base64":   {goodP256, "!!!!!!!!!!!!!!!!!!!!!!"},
		"empty":        {"", ""},
	} {
		if _, err := ParseSubscription(ep, k[0], k[1], nil); !errors.Is(err, ErrKeys) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ParseSubscription("http://evil/x", goodP256, goodAuth, nil); !errors.Is(err, ErrEndpoint) {
		t.Errorf("endpoint: %v", err)
	}
}
