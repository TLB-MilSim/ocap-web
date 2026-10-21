package server

// TLB suite sign-in ("tlb" auth mode).
//
// The TLB Login Service signs users in with Discord and sets a shared session
// cookie (tlb_session, an ES256 JWT) for all TLB sites. TLBLogin exchanges that
// session for OCAP's own token, so the existing viewer/admin checks keep working:
//
//	"member" permission    -> viewer
//	"ocap.edit" permission -> admin
//
// Every decision is checked live against the Login Service's /verify endpoint
// (session still active, account not blocked, current Discord roles), cached for
// tlbLiveTTL. Signing out of the suite, a block or losing a role therefore takes
// effect within tlbLiveTTL, not when OCAP's own token expires. If the Login
// Service can't be reached, access is denied (fail closed).
//
// In this mode OCAP's own Steam and password logins are disabled.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	// tlbLiveTTL is how long a live /verify answer is reused for the same session cookie.
	tlbLiveTTL = 30 * time.Second
	// tlbMaxStaleKeys is how long cached signing keys stay trusted when the JWKS can't be refreshed.
	tlbMaxStaleKeys = 24 * time.Hour
)

type tlbClaims struct {
	Name    string   `json:"name"`
	Avatar  string   `json:"avatar"`
	SteamID string   `json:"steam_id"`
	Perms   []string `json:"perms"`
	jwt.RegisteredClaims
}

// ---- Signing keys (JWKS) ----------------------------------------------------

// tlbKeySet caches the Login Service's public suite keys.
type tlbKeySet struct {
	mu          sync.Mutex
	url         string
	keys        map[string]*ecdsa.PublicKey
	fetchedAt   time.Time // last successful fetch
	attemptedAt time.Time // last attempt, successful or not (throttles refetches)
}

var tlbKeys = &tlbKeySet{}

func (s *tlbKeySet) key(jwksURL, kid string) (*ecdsa.PublicKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.url != jwksURL {
		s.url, s.keys, s.fetchedAt, s.attemptedAt = jwksURL, nil, time.Time{}, time.Time{}
	}
	_, known := s.keys[kid]
	due := time.Since(s.fetchedAt) > 10*time.Minute || !known
	// Refetch at most every 30s, whether the last attempt failed or the kid is unknown,
	// so neither an outage nor tokens with random key IDs cause a fetch per request.
	if due && time.Since(s.attemptedAt) > 30*time.Second {
		s.attemptedAt = time.Now()
		if keys, err := fetchTLBKeys(jwksURL); err != nil {
			log.Printf("WARN: TLB JWKS refresh failed: %v", err)
		} else {
			s.keys, s.fetchedAt = keys, time.Now()
		}
	}
	if s.keys == nil || time.Since(s.fetchedAt) > tlbMaxStaleKeys {
		return nil, errors.New("TLB signing keys unavailable")
	}
	k, ok := s.keys[kid]
	if !ok {
		return nil, fmt.Errorf("unknown TLB signing key %q", kid)
	}
	return k, nil
}

func fetchTLBKeys(jwksURL string) (map[string]*ecdsa.PublicKey, error) {
	client := &http.Client{Timeout: 3 * time.Second}
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
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

// tlbVerifyURL is the configured live-check address, or the public one.
func tlbVerifyURL(cfg TLBAuth) string {
	if cfg.VerifyURL != "" {
		return cfg.VerifyURL
	}
	return cfg.Issuer() + "verify"
}

// tlbSession returns the signature-checked TLB session from the request cookie, or nil.
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
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return nil
	}
	return claims
}

// ---- Live check (/verify) ---------------------------------------------------

// tlbLiveResult is the Login Service's /verify answer for a session.
type tlbLiveResult struct {
	DiscordID string   `json:"discord_id"`
	SteamID   string   `json:"steam_id"`
	Perms     []string `json:"perms"`
}

type tlbLiveEntry struct {
	result  *tlbLiveResult // nil = no access
	expires time.Time
}

// tlbLiveCache remembers /verify answers per session cookie (by SHA-256, never the cookie itself).
type tlbLiveCache struct {
	mu      sync.Mutex
	entries map[string]tlbLiveEntry
}

var tlbLive = &tlbLiveCache{entries: map[string]tlbLiveEntry{}}

var tlbVerifyClient = &http.Client{
	Timeout: 3 * time.Second,
	// /verify answers directly; never follow redirects with the session cookie.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// tlbVerifyLive asks the Login Service whether the request's suite session is still valid
// and returns its current identity and permissions, or nil when there is no access
// (signed out, blocked, no TLB role, or the Login Service unreachable).
func (h *Handler) tlbVerifyLive(r *http.Request) *tlbLiveResult {
	cookie, err := r.Cookie(tlbSessionCookie)
	if err != nil || cookie.Value == "" {
		return nil
	}
	sum := sha256.Sum256([]byte(cookie.Value))
	key := hex.EncodeToString(sum[:])
	now := time.Now()

	tlbLive.mu.Lock()
	if e, ok := tlbLive.entries[key]; ok && now.Before(e.expires) {
		tlbLive.mu.Unlock()
		return e.result
	}
	tlbLive.mu.Unlock()

	result, cacheable := h.fetchTLBVerify(r, cookie.Value)
	if cacheable {
		tlbLive.mu.Lock()
		if len(tlbLive.entries) > 10000 {
			for k, e := range tlbLive.entries {
				if now.After(e.expires) {
					delete(tlbLive.entries, k)
				}
			}
			if len(tlbLive.entries) > 10000 {
				tlbLive.entries = map[string]tlbLiveEntry{}
			}
		}
		tlbLive.entries[key] = tlbLiveEntry{result: result, expires: now.Add(tlbLiveTTL)}
		tlbLive.mu.Unlock()
	}
	return result
}

// fetchTLBVerify calls /verify. cacheable is false for outages, so they are retried.
func (h *Handler) fetchTLBVerify(r *http.Request, sessionToken string) (result *tlbLiveResult, cacheable bool) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, tlbVerifyURL(h.setting.Auth.TLB), nil)
	if err != nil {
		return nil, false
	}
	req.AddCookie(&http.Cookie{Name: tlbSessionCookie, Value: sessionToken})
	resp, err := tlbVerifyClient.Do(req)
	if err != nil {
		log.Printf("WARN: TLB /verify unreachable: %v", err)
		return nil, false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		var res tlbLiveResult
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&res); err != nil || res.DiscordID == "" {
			return nil, false
		}
		return &res, true
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, true
	default:
		log.Printf("WARN: TLB /verify answered HTTP %d", resp.StatusCode)
		return nil, false
	}
}

// tlbSubject is the OCAP token subject for a TLB identity.
func tlbSubject(steamID, discordID string) string {
	if steamID != "" {
		return steamID
	}
	return "discord:" + discordID
}

// tlbAllows reports whether, in tlb mode, the request's live suite session belongs to the
// same person as the OCAP token (subject) and holds perm. Outside tlb mode it allows.
func (h *Handler) tlbAllows(r *http.Request, subject, perm string) bool {
	if h.setting.Auth.Mode != "tlb" {
		return true
	}
	live := h.tlbVerifyLive(r)
	return live != nil &&
		tlbSubject(live.SteamID, live.DiscordID) == subject &&
		slices.Contains(live.Perms, perm)
}

// ---- Login exchange ---------------------------------------------------------

// TLBLogin exchanges the shared TLB session for an OCAP token (see file comment).
func (h *Handler) TLBLogin(w http.ResponseWriter, r *http.Request) {
	if h.setting.Auth.Mode != "tlb" {
		http.Error(w, "TLB login not enabled", http.StatusNotFound)
		return
	}

	session := h.tlbSession(r)
	if session == nil {
		// Not signed in: TLB sign-in, then back here. The Login Service only returns
		// to its configured origins, so a spoofed Host can't redirect elsewhere.
		prefix := strings.TrimRight(h.setting.PrefixURL, "/")
		self := requestScheme(r) + "://" + requestHost(r) + prefix + "/api/v1/auth/tlb"
		http.Redirect(w, r, h.setting.Auth.TLB.Issuer()+"login?returnUrl="+url.QueryEscape(self), http.StatusTemporaryRedirect)
		return
	}
	live := h.tlbVerifyLive(r)
	if live == nil || live.DiscordID != session.Subject || !slices.Contains(live.Perms, tlbPermMember) {
		h.authRedirect(w, r, "auth_error=not_allowed")
		return
	}

	role := "viewer"
	if slices.Contains(live.Perms, tlbPermAdmin) {
		role = "admin"
	}
	opts := []ClaimOption{WithRole(role), WithSteamProfile(session.Name, session.Avatar)}
	if session.ExpiresAt != nil {
		// Never outlive the suite session it was exchanged from.
		opts = append(opts, WithExpiresBefore(session.ExpiresAt.Time))
	}
	token, err := h.jwt.Create(tlbSubject(live.SteamID, live.DiscordID), opts...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// In the URL fragment, which browsers never send to servers or put in access logs.
	h.authRedirectFragment(w, r, "auth_token="+token)
}

// authRedirectFragment redirects to the frontend root with a URL fragment.
func (h *Handler) authRedirectFragment(w http.ResponseWriter, r *http.Request, fragment string) {
	prefix := strings.TrimRight(h.setting.PrefixURL, "/") + "/"
	http.Redirect(w, r, prefix+"#"+fragment, http.StatusTemporaryRedirect)
}
