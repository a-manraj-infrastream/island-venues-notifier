package pushauth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/idtoken"
)

const (
	pushSA   = "app-23ff2ba10851@island-venues-27e7.iam.gserviceaccount.com"
	goodTok  = "good-token"
	audience = "https://island-venues-notifier-abc-uc.a.run.app"
)

// fakeVerifier accepts goodTok for audience wantAud and returns claims.
type fakeVerifier struct {
	wantAud string
	issuer  string
	claims  map[string]any
	seen    []string
}

func (f *fakeVerifier) Validate(_ context.Context, token, aud string) (*idtoken.Payload, error) {
	f.seen = append(f.seen, aud)
	if token != goodTok || aud != f.wantAud {
		return nil, errors.New("invalid")
	}
	return &idtoken.Payload{Issuer: f.issuer, Audience: aud, Claims: f.claims}, nil
}

func okClaims() map[string]any {
	return map[string]any{"email": pushSA, "email_verified": true}
}

func quiet() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// serve runs one request through Require and reports the status and whether
// the wrapped handler ran (it must not run, nor the body be read, on 401).
func serve(t *testing.T, v Verifier, cfg Config, host, authz string) (int, bool) {
	t.Helper()
	ran := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	})
	body := &trackingReader{r: strings.NewReader(`{"message":{}}`)}
	req := httptest.NewRequest(http.MethodPost, "/", body)
	req.Host = host
	if authz != "" {
		req.Header.Set("Authorization", authz)
	}
	rec := httptest.NewRecorder()
	Require(v, cfg, quiet(), next).ServeHTTP(rec, req)
	if rec.Code == http.StatusUnauthorized && body.read {
		t.Errorf("body was read before the request was rejected")
	}
	return rec.Code, ran
}

type trackingReader struct {
	r    io.Reader
	read bool
}

func (t *trackingReader) Read(p []byte) (int, error) { t.read = true; return t.r.Read(p) }

func TestRequire(t *testing.T) {
	cfg := Config{ServiceAccount: pushSA, Audiences: []string{audience}}
	cases := []struct {
		name    string
		v       *fakeVerifier
		cfg     Config
		host    string
		authz   string
		wantRun bool
	}{
		{"valid token", &fakeVerifier{wantAud: audience, issuer: "https://accounts.google.com", claims: okClaims()}, cfg, "x", "Bearer " + goodTok, true},
		{"lowercase scheme", &fakeVerifier{wantAud: audience, issuer: "accounts.google.com", claims: okClaims()}, cfg, "x", "bearer " + goodTok, true},
		{"missing header", &fakeVerifier{wantAud: audience, issuer: "https://accounts.google.com", claims: okClaims()}, cfg, "x", "", false},
		{"not bearer", &fakeVerifier{wantAud: audience, issuer: "https://accounts.google.com", claims: okClaims()}, cfg, "x", "Basic " + goodTok, false},
		{"empty bearer", &fakeVerifier{wantAud: audience, issuer: "https://accounts.google.com", claims: okClaims()}, cfg, "x", "Bearer   ", false},
		{"wrong audience", &fakeVerifier{wantAud: "https://elsewhere.a.run.app", issuer: "https://accounts.google.com", claims: okClaims()}, cfg, "x", "Bearer " + goodTok, false},
		{"bad token", &fakeVerifier{wantAud: audience, issuer: "https://accounts.google.com", claims: okClaims()}, cfg, "x", "Bearer forged", false},
		{"wrong email", &fakeVerifier{wantAud: audience, issuer: "https://accounts.google.com", claims: map[string]any{"email": "attacker@evil.iam.gserviceaccount.com", "email_verified": true}}, cfg, "x", "Bearer " + goodTok, false},
		{"no email", &fakeVerifier{wantAud: audience, issuer: "https://accounts.google.com", claims: map[string]any{"email_verified": true}}, cfg, "x", "Bearer " + goodTok, false},
		{"unverified email", &fakeVerifier{wantAud: audience, issuer: "https://accounts.google.com", claims: map[string]any{"email": pushSA, "email_verified": false}}, cfg, "x", "Bearer " + goodTok, false},
		{"foreign issuer", &fakeVerifier{wantAud: audience, issuer: "https://cloud.google.com/iap", claims: okClaims()}, cfg, "x", "Bearer " + goodTok, false},
		{"audience from host", &fakeVerifier{wantAud: "https://notifier.a.run.app", issuer: "https://accounts.google.com", claims: okClaims()}, Config{ServiceAccount: pushSA}, "notifier.a.run.app", "Bearer " + goodTok, true},
		{"host audience mismatch", &fakeVerifier{wantAud: "https://notifier.a.run.app", issuer: "https://accounts.google.com", claims: okClaims()}, Config{ServiceAccount: pushSA}, "other.a.run.app", "Bearer " + goodTok, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, ran := serve(t, c.v, c.cfg, c.host, c.authz)
			if ran != c.wantRun {
				t.Fatalf("handler ran = %v, want %v (status %d)", ran, c.wantRun, code)
			}
			want := http.StatusOK
			if !c.wantRun {
				want = http.StatusUnauthorized
			}
			if code != want {
				t.Fatalf("status = %d, want %d", code, want)
			}
		})
	}
}

// TestRequireTriesEveryAudience: several audiences may be configured (for
// example the run.app URL and a custom one); any of them is enough.
func TestRequireTriesEveryAudience(t *testing.T) {
	v := &fakeVerifier{wantAud: audience, issuer: "https://accounts.google.com", claims: okClaims()}
	cfg := Config{ServiceAccount: pushSA, Audiences: []string{"https://first.example", audience}}
	if code, ran := serve(t, v, cfg, "x", "Bearer "+goodTok); !ran || code != http.StatusOK {
		t.Fatalf("status %d ran %v, want 200 and run", code, ran)
	}
	if len(v.seen) != 2 {
		t.Fatalf("audiences tried = %v, want both", v.seen)
	}
}

// TestRequireRejectsWithoutCallingTheVerifier: a request with no usable
// bearer token is refused before any token validation (which would fetch
// Google's certificates) and before the body is read.
func TestRequireRejectsWithoutCallingTheVerifier(t *testing.T) {
	for _, authz := range []string{"", "Basic abc", "Bearer ", "Bearer    "} {
		v := &fakeVerifier{wantAud: audience, issuer: "https://accounts.google.com", claims: okClaims()}
		code, ran := serve(t, v, Config{ServiceAccount: pushSA, Audiences: []string{audience}}, "x", authz)
		if ran || code != http.StatusUnauthorized {
			t.Fatalf("%q: status %d ran %v, want 401", authz, code, ran)
		}
		if len(v.seen) != 0 {
			t.Fatalf("%q: verifier called for %v, want no call", authz, v.seen)
		}
	}
}
