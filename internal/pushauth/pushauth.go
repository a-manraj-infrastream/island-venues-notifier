// Package pushauth admits only Eventarc push deliveries to the notifier.
//
// The engine deploys Cloud Run with its invoker IAM check disabled, so the
// platform does not stop anyone who can reach the service from calling it.
// Without this check, anything inside the VPC could POST an event and make
// the notifier e-mail any address. Eventarc signs every push with a
// Google-issued OIDC token for the trigger's service account; this package
// verifies that token before the request body is read.
package pushauth

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"google.golang.org/api/idtoken"
)

// Verifier checks a Google-signed ID token's signature, expiry and audience.
// *idtoken.Validator satisfies it; tests use a fake so they never reach Google.
type Verifier interface {
	Validate(ctx context.Context, token, audience string) (*idtoken.Payload, error)
}

// googleIssuers are the issuers Google uses for service-account ID tokens.
// idtoken.Validate checks the signature but not the issuer, so it is pinned
// here: a token minted by any other issuer is not an Eventarc push.
var googleIssuers = map[string]bool{
	"https://accounts.google.com": true,
	"accounts.google.com":         true,
}

// Config is what a push must prove.
type Config struct {
	// ServiceAccount is the e-mail of the Eventarc trigger's service account.
	// Only tokens for this identity are accepted.
	ServiceAccount string
	// Audiences are the accepted token audiences. When empty, the audience is
	// derived from the request, as "https://" + Host: Eventarc addresses a
	// Cloud Run destination by the service URL, which is only known once the
	// service exists. The service-account pin is the actual control either way.
	Audiences []string
}

// Require wraps next so it only runs for a verified Eventarc push.
// Every failure answers 401 without reading the body.
func Require(v Verifier, cfg Config, logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r.Header.Get("Authorization"))
		if !ok {
			reject(w, logger, r, "missing bearer token")
			return
		}
		audiences := cfg.Audiences
		if len(audiences) == 0 {
			audiences = []string{"https://" + r.Host}
		}
		var payload *idtoken.Payload
		for _, aud := range audiences {
			p, err := v.Validate(r.Context(), token, aud)
			if err == nil && p != nil {
				payload = p
				break
			}
		}
		if payload == nil {
			reject(w, logger, r, "token failed validation for every accepted audience")
			return
		}
		if !googleIssuers[payload.Issuer] {
			reject(w, logger, r, "token issuer is not Google")
			return
		}
		email, _ := payload.Claims["email"].(string)
		if email == "" || !strings.EqualFold(email, cfg.ServiceAccount) {
			reject(w, logger, r, "token is not for the Eventarc service account")
			return
		}
		if verified, _ := payload.Claims["email_verified"].(bool); !verified {
			reject(w, logger, r, "token e-mail is not verified")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearer extracts the token from an "Authorization: Bearer <token>" header.
func bearer(h string) (string, bool) {
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	return token, token != ""
}

// reject answers 401. The token itself is never logged.
func reject(w http.ResponseWriter, logger *slog.Logger, r *http.Request, reason string) {
	logger.WarnContext(r.Context(), "push rejected", slog.String("reason", reason))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
}
