package server

// TLB suite sign-in ("tlb" auth mode).
//
// The TLB auth service signs users in with Discord and sets a shared session
// cookie (tlb_session, an ES256 JWT) for all TLB sites. TLBLogin exchanges that
// session for OCAP's own token, so the existing viewer/admin checks keep working:
//
//	"member" permission    -> viewer
//	"ocap.edit" permission -> admin
//
// Without a valid session it sends the browser to the TLB sign-in and back.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	tlbSessionCookie = "tlb_session"
	tlbAudience      = "tlb-suite"
	tlbPermMember    = "member"
	tlbPermAdmin     = "ocap.edit"
)

type tlbClaims struct {
	Name    string   `json:"name"`
	Avatar  string   `json:"avatar"`
	SteamID string   `json:"steam_id"`
	Perms   []string `json:"perms"`
	jwt.RegisteredClaims
}

// tlbKeySet caches the auth service's public keys (JWKS).
type tlbKeySet struct {
	mu        sync.Mutex
	url       string
	keys      map[string]*ecdsa.PublicKey
	fetchedAt time.Time
}

var tlbKeys = &tlbKeySet{}

func (s *tlbKeySet) key(jwksURL, kid string) (*ecdsa.PublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stale := s.url != jwksURL || time.Since(s.fetchedAt) > 10*time.Minute
	// Unknown key ID: refetch (key rotation), at most every 30s.
	if _, ok := s.keys[kid]; !ok && time.Since(s.fetchedAt) > 30*time.Second {
		stale = true
	}
	if stale {
		keys, err := fetchTLBKeys(jwksURL)
		if err != nil {
			if s.keys == nil || s.url != jwksURL {
				return nil, err
			}
			log.Printf("WARN: TLB JWKS refresh failed, using cached keys: %v", err)
		} else {
			s.url, s.keys, s.fetchedAt = jwksURL, keys, time.Now()
		}
	}
	k, ok := s.keys[kid]
	if !ok {
		return nil, fmt.Errorf("unknown TLB signing key %q", kid)
	}
	return k, nil
}

func fetchTLBKeys(jwksURL string) (map[string]*ecdsa.PublicKey, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(jwksURL)
	if err != nil {
		return nil, fmt.Errorf("fetch TLB JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch TLB JWKS: HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			Kty, Crv, Kid, X, Y string
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode TLB JWKS: %w", err)
	}
	keys := make(map[string]*ecdsa.PublicKey)
	for _, k := range doc.Keys {
		if k.Kty != "EC" || k.Crv != "P-256" {
			continue
		}
		x, errX := base64.RawURLEncoding.DecodeString(k.X)
		y, errY := base64.RawURLEncoding.DecodeString(k.Y)
		if errX != nil || errY != nil {
			continue
		}
		keys[k.Kid] = &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	}
	if len(keys) == 0 {
		return nil, errors.New("TLB JWKS has no usable keys")
	}
	return keys, nil
}

// tlbJwksURL is the configured JWKS address, or the public one (inside Docker,
// set auth.tlb.jwksUrl to the internal address, e.g. http://auth:8080/...).
func tlbJwksURL(cfg TLBAuth) string {
	if cfg.JwksURL != "" {
		return cfg.JwksURL
	}
	return cfg.Issuer() + ".well-known/jwks.json"
}

// tlbSession returns the verified TLB session from the request cookie, or nil.
func (h *Handler) tlbSession(r *http.Request) *tlbClaims {
	cookie, err := r.Cookie(tlbSessionCookie)
	if err != nil || cookie.Value == "" {
		return nil
	}
	cfg := h.setting.Auth.TLB
	claims := &tlbClaims{}
	_, err = jwt.ParseWithClaims(cookie.Value, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return tlbKeys.key(tlbJwksURL(cfg), kid)
	},
		jwt.WithValidMethods([]string{"ES256"}),
		jwt.WithIssuer(cfg.Issuer()),
		jwt.WithAudience(tlbAudience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(time.Minute),
	)
	if err != nil {
		return nil
	}
	return claims
}

// TLBLogin exchanges the shared TLB session for an OCAP token (see file comment).
func (h *Handler) TLBLogin(w http.ResponseWriter, r *http.Request) {
	if h.setting.Auth.Mode != "tlb" {
		http.Error(w, "TLB login not enabled", http.StatusNotFound)
		return
	}

	session := h.tlbSession(r)
	if session == nil {
		// Not signed in: TLB sign-in, then back here.
		prefix := strings.TrimRight(h.setting.PrefixURL, "/")
		self := requestScheme(r) + "://" + requestHost(r) + prefix + "/api/v1/auth/tlb"
		http.Redirect(w, r, h.setting.Auth.TLB.Issuer()+"login?returnUrl="+url.QueryEscape(self), http.StatusTemporaryRedirect)
		return
	}
	if !slices.Contains(session.Perms, tlbPermMember) {
		h.authRedirect(w, r, "auth_error=not_allowed")
		return
	}

	role := "viewer"
	if slices.Contains(session.Perms, tlbPermAdmin) {
		role = "admin"
	}
	subject := session.SteamID
	if subject == "" {
		subject = "discord:" + session.Subject
	}
	token, err := h.jwt.Create(subject, WithRole(role), WithSteamProfile(session.Name, session.Avatar))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.authRedirect(w, r, "auth_token="+token)
}
