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

// fakeTLBAuth serves a JWKS for a fresh P-256 key and mints session tokens with it.
type fakeTLBAuth struct {
	key    *ecdsa.PrivateKey
	server *httptest.Server
}

func newFakeTLBAuth(t *testing.T) *fakeTLBAuth {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	f := &fakeTLBAuth{key: key}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "EC", "crv": "P-256", "kid": "test-kid",
			"x": base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, 32))),
			"y": base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, 32))),
		}}})
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
				TLB:        TLBAuth{PublicURL: f.issuer(), JwksURL: f.server.URL},
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
	claims := hdlr.jwt.Claims(u.Query().Get("auth_token"))
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
