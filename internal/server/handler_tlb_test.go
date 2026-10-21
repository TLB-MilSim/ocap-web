package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeTLBAuth serves a JWKS for a fresh P-256 key and a /verify live check, and mints
// session tokens with the key. By default /verify answers with the session's own claims;
// set live to answer differently (e.g. a role removed since the session was issued).
type fakeTLBAuth struct {
	key    *ecdsa.PrivateKey
	server *httptest.Server
	live   func(tlbClaims) (int, *tlbLiveResult)
}

func newFakeTLBAuth(t *testing.T) *fakeTLBAuth {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	f := &fakeTLBAuth{key: key}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/verify" {
			json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kty": "EC", "crv": "P-256", "kid": "test-kid",
				"x": base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
				"y": base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
			}}})
			return
		}
		cookie, err := r.Cookie(tlbSessionCookie)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		claims := tlbClaims{}
		if _, err := jwt.ParseWithClaims(cookie.Value, &claims, func(*jwt.Token) (any, error) { return &key.PublicKey, nil }); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		status, res := http.StatusOK, &tlbLiveResult{DiscordID: claims.Subject, SteamID: claims.SteamID, Perms: claims.Perms}
		if f.live != nil {
			status, res = f.live(claims)
		}
		w.WriteHeader(status)
		if res != nil {
			json.NewEncoder(w).Encode(res)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeTLBAuth) issuer() string { return "https://auth.example.test/" }

func (f *fakeTLBAuth) token(t *testing.T, signer *ecdsa.PrivateKey, claims tlbClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = "test-kid"
	s, err := tok.SignedString(signer)
	require.NoError(t, err)
	return s
}

func (f *fakeTLBAuth) claims(perms []string, steamID string) tlbClaims {
	return tlbClaims{
		Name: "W. Fosse", Avatar: "https://cdn.discordapp.com/avatars/1/a.png", SteamID: steamID, Perms: perms,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "123456789012345678",
			ID:        rand.Text(), // unique per session, so live-check cache entries never collide
			Issuer:    f.issuer(),
			Audience:  jwt.ClaimStrings{tlbAudience},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
}

func newTLBHandler(f *fakeTLBAuth) *Handler {
	return &Handler{
		setting: Setting{
			Secret: "test-secret",
			Auth: Auth{
				Mode:       "tlb",
				SessionTTL: time.Hour,
				TLB:        TLBAuth{PublicURL: f.issuer(), JwksURL: f.server.URL + "/jwks", VerifyURL: f.server.URL + "/verify"},
			},
		},
		jwt: NewJWTManager("test-secret", time.Hour),
	}
}

func tlbLogin(hdlr *Handler, sessionToken string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://ocap.example.test/api/v1/auth/tlb", nil)
	if sessionToken != "" {
		req.AddCookie(&http.Cookie{Name: tlbSessionCookie, Value: sessionToken})
	}
	rec := httptest.NewRecorder()
	hdlr.TLBLogin(rec, req)
	return rec
}

func ocapTokenFrom(t *testing.T, hdlr *Handler, rec *httptest.ResponseRecorder) *SteamClaims {
	t.Helper()
	require.Equal(t, http.StatusTemporaryRedirect, rec.Code)
	u, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	assert.Empty(t, u.RawQuery, "the token must not be in the query string")
	frag, err := url.ParseQuery(u.Fragment)
	require.NoError(t, err)
	claims := hdlr.jwt.Claims(frag.Get("auth_token"))
	require.NotNil(t, claims, "expected an OCAP token in %s", rec.Header().Get("Location"))
	return claims
}

func TestTLBLogin_NoSessionRedirectsToSignIn(t *testing.T) {
	f := newFakeTLBAuth(t)
	rec := tlbLogin(newTLBHandler(f), "")
	assert.Equal(t, http.StatusTemporaryRedirect, rec.Code)
	assert.Equal(t,
		"https://auth.example.test/login?returnUrl="+url.QueryEscape("http://ocap.example.test/api/v1/auth/tlb"),
		rec.Header().Get("Location"))
}

func TestTLBLogin_MemberGetsViewer(t *testing.T) {
	f := newFakeTLBAuth(t)
	hdlr := newTLBHandler(f)
	claims := ocapTokenFrom(t, hdlr, tlbLogin(hdlr, f.token(t, f.key, f.claims([]string{"member"}, "76561198012345678"))))
	assert.Equal(t, "viewer", claims.Role)
	assert.Equal(t, "76561198012345678", claims.Subject)
	assert.Equal(t, "W. Fosse", claims.SteamName)
}

func TestTLBLogin_OcapEditGetsAdmin(t *testing.T) {
	f := newFakeTLBAuth(t)
	hdlr := newTLBHandler(f)
	claims := ocapTokenFrom(t, hdlr, tlbLogin(hdlr, f.token(t, f.key, f.claims([]string{"member", "ocap.edit"}, "76561198012345678"))))
	assert.Equal(t, "admin", claims.Role)
}

func TestTLBLogin_NoSteamLinkUsesDiscordSubject(t *testing.T) {
	f := newFakeTLBAuth(t)
	hdlr := newTLBHandler(f)
	claims := ocapTokenFrom(t, hdlr, tlbLogin(hdlr, f.token(t, f.key, f.claims([]string{"member"}, ""))))
	assert.Equal(t, "discord:123456789012345678", claims.Subject)
}

func TestTLBLogin_WithoutMemberIsNotAllowed(t *testing.T) {
	f := newFakeTLBAuth(t)
	rec := tlbLogin(newTLBHandler(f), f.token(t, f.key, f.claims([]string{}, "76561198012345678")))
	assert.Equal(t, http.StatusTemporaryRedirect, rec.Code)
	assert.Equal(t, "/?auth_error=not_allowed", rec.Header().Get("Location"))
}

func TestTLBLogin_RejectsTokenSignedByAnotherKey(t *testing.T) {
	f := newFakeTLBAuth(t)
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	rec := tlbLogin(newTLBHandler(f), f.token(t, other, f.claims([]string{"member", "ocap.edit"}, "1")))
	assert.Contains(t, rec.Header().Get("Location"), "https://auth.example.test/login?", "forged session must be treated as signed out")
}

func TestTLBLogin_RejectsWrongAudienceAndExpired(t *testing.T) {
	f := newFakeTLBAuth(t)
	hdlr := newTLBHandler(f)

	wrongAud := f.claims([]string{"member"}, "1")
	wrongAud.Audience = jwt.ClaimStrings{"someone-else"}
	assert.Contains(t, tlbLogin(hdlr, f.token(t, f.key, wrongAud)).Header().Get("Location"), "/login?")

	expired := f.claims([]string{"member"}, "1")
	expired.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
	assert.Contains(t, tlbLogin(hdlr, f.token(t, f.key, expired)).Header().Get("Location"), "/login?")
}

func TestTLBLogin_DisabledOutsideTLBMode(t *testing.T) {
	f := newFakeTLBAuth(t)
	hdlr := newTLBHandler(f)
	hdlr.setting.Auth.Mode = "public"
	assert.Equal(t, http.StatusNotFound, tlbLogin(hdlr, "").Code)
}

func TestTLBLogin_TokenNeverOutlivesSuiteSession(t *testing.T) {
	f := newFakeTLBAuth(t)
	hdlr := newTLBHandler(f)
	hdlr.jwt = NewJWTManager("test-secret", 12*time.Hour)
	session := f.claims([]string{"member"}, "1")
	session.ExpiresAt = jwt.NewNumericDate(time.Now().Add(20 * time.Minute))
	claims := ocapTokenFrom(t, hdlr, tlbLogin(hdlr, f.token(t, f.key, session)))
	assert.WithinDuration(t, session.ExpiresAt.Time, claims.ExpiresAt.Time, time.Second)
}

func TestTLBLogin_UsesLivePermissions(t *testing.T) {
	f := newFakeTLBAuth(t)
	hdlr := newTLBHandler(f)
	// The session still says ocap.edit, but HQ was removed since it was issued.
	f.live = func(c tlbClaims) (int, *tlbLiveResult) {
		return http.StatusOK, &tlbLiveResult{DiscordID: c.Subject, SteamID: c.SteamID, Perms: []string{"member"}}
	}
	claims := ocapTokenFrom(t, hdlr, tlbLogin(hdlr, f.token(t, f.key, f.claims([]string{"member", "ocap.edit"}, "1"))))
	assert.Equal(t, "viewer", claims.Role)
}

func TestTLBLogin_FailsClosedWhenLoginServiceDown(t *testing.T) {
	f := newFakeTLBAuth(t)
	hdlr := newTLBHandler(f)
	f.live = func(tlbClaims) (int, *tlbLiveResult) { return http.StatusServiceUnavailable, nil }
	rec := tlbLogin(hdlr, f.token(t, f.key, f.claims([]string{"member"}, "1")))
	assert.Equal(t, "/?auth_error=not_allowed", rec.Header().Get("Location"))
}

func TestSteamLogin_DisabledInTLBMode(t *testing.T) {
	hdlr := newTLBHandler(newFakeTLBAuth(t))
	for _, h := range []http.HandlerFunc{hdlr.SteamLogin, hdlr.SteamCallback} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "http://ocap.example.test/api/v1/auth/steam", nil))
		assert.Equal(t, http.StatusNotFound, rec.Code)
	}
}

// tlbRequest is an API request with an OCAP bearer token and, optionally, the suite session cookie.
func tlbRequest(ocapToken, sessionToken string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "http://ocap.example.test/api/v1/operations", nil)
	req.Header.Set("Authorization", "Bearer "+ocapToken)
	if sessionToken != "" {
		req.AddCookie(&http.Cookie{Name: tlbSessionCookie, Value: sessionToken})
	}
	return req
}

func TestTLBMiddleware_ChecksLiveSession(t *testing.T) {
	f := newFakeTLBAuth(t)
	hdlr := newTLBHandler(f)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	serve := func(mw func(http.Handler) http.Handler, req *http.Request) int {
		rec := httptest.NewRecorder()
		mw(ok).ServeHTTP(rec, req)
		return rec.Code
	}
	adminToken, err := hdlr.jwt.Create("76561198012345678", WithRole("admin"))
	require.NoError(t, err)
	adminSession := f.token(t, f.key, f.claims([]string{"member", "ocap.edit"}, "76561198012345678"))

	assert.Equal(t, http.StatusNoContent, serve(hdlr.requireViewer, tlbRequest(adminToken, adminSession)))
	assert.Equal(t, http.StatusNoContent, serve(hdlr.requireAdmin, tlbRequest(adminToken, adminSession)))

	// Signed out of the suite: the OCAP token alone is not enough.
	assert.Equal(t, http.StatusUnauthorized, serve(hdlr.requireViewer, tlbRequest(adminToken, "")))
	assert.Equal(t, http.StatusUnauthorized, serve(hdlr.requireAdmin, tlbRequest(adminToken, "")))

	// Someone else's suite session doesn't unlock this OCAP token.
	otherSession := f.token(t, f.key, f.claims([]string{"member", "ocap.edit"}, "76561198099999999"))
	assert.Equal(t, http.StatusUnauthorized, serve(hdlr.requireAdmin, tlbRequest(adminToken, otherSession)))

	// HQ role removed: still a viewer, no longer an admin.
	f.live = func(c tlbClaims) (int, *tlbLiveResult) {
		return http.StatusOK, &tlbLiveResult{DiscordID: c.Subject, SteamID: c.SteamID, Perms: []string{"member"}}
	}
	demoted := f.token(t, f.key, f.claims([]string{"member", "ocap.edit"}, "76561198012345678"))
	assert.Equal(t, http.StatusNoContent, serve(hdlr.requireViewer, tlbRequest(adminToken, demoted)))
	assert.Equal(t, http.StatusUnauthorized, serve(hdlr.requireAdmin, tlbRequest(adminToken, demoted)))

	// Blocked or signed out at the Login Service.
	f.live = func(tlbClaims) (int, *tlbLiveResult) { return http.StatusUnauthorized, nil }
	revoked := f.token(t, f.key, f.claims([]string{"member"}, "76561198012345678"))
	assert.Equal(t, http.StatusUnauthorized, serve(hdlr.requireViewer, tlbRequest(adminToken, revoked)))
}

func TestJWT_RequiresExpiry(t *testing.T) {
	m := NewJWTManager("test-secret", time.Hour)
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, SteamClaims{Role: "admin"}).SignedString([]byte("test-secret"))
	require.NoError(t, err)
	assert.Error(t, m.Validate(tok))
	assert.Nil(t, m.Claims(tok))
}

func TestLoadOrCreateJWTKey(t *testing.T) {
	path := t.TempDir() + "/jwt.key"
	first, err := loadOrCreateJWTKey(path)
	require.NoError(t, err)
	assert.Len(t, first, 64)
	again, err := loadOrCreateJWTKey(path)
	require.NoError(t, err)
	assert.Equal(t, first, again, "the key must survive restarts")
}
