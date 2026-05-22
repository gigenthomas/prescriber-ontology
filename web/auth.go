package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// ── Config ──────────────────────────────────────────────────────────────────

type authConfig struct {
	Provider     string // "keycloak" | "none"
	IssuerURL    string // what the JWT's iss claim is expected to be — must match Keycloak's KC_HOSTNAME
	DiscoveryURL string // where the bot fetches /.well-known/openid-configuration; defaults to IssuerURL but can differ in docker (compose-internal URL vs browser-facing URL)
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

var (
	authCfg          authConfig
	oidcProvider     *oidc.Provider
	oauthConfig      oauth2.Config
	oidcVerifier     *oidc.IDTokenVerifier
	sessionStore     sync.Map // sessionID -> *AuthenticatedUser
	pendingStates    sync.Map // state -> stateMeta (TTL'd via janitor)
	authInitialized  bool
)

type stateMeta struct {
	createdAt time.Time
	returnTo  string
}

type AuthenticatedUser struct {
	SessionID         string
	Subject           string    // sub claim
	Username          string    // preferred_username
	Email             string
	Name              string
	Roles             []string
	AccessToken       string
	IDToken           string
	RefreshToken      string
	ExpiresAt         time.Time
	cacheRefreshedAt  time.Time // last time we wrote this user to user_cache
}

func loadAuthConfig() {
	issuer := getenv("KEYCLOAK_ISSUER_URL", "http://localhost:8180/realms/ontology-dev")
	authCfg = authConfig{
		Provider:     getenv("AUTH_PROVIDER", "none"),
		IssuerURL:    issuer,
		DiscoveryURL: getenv("KEYCLOAK_DISCOVERY_URL", issuer),
		ClientID:     getenv("KEYCLOAK_CLIENT_ID", "ontology-web"),
		ClientSecret: getenv("KEYCLOAK_CLIENT_SECRET", ""), // public client → empty
		RedirectURL:  getenv("KEYCLOAK_REDIRECT_URL", "http://localhost:8081/auth/callback"),
	}
}

func authEnabled() bool {
	return authCfg.Provider == "keycloak"
}

// initAuth performs OIDC discovery against the configured issuer and prepares
// the verifier + oauth2 config. Safe to call when AUTH_PROVIDER=none — in
// that case it's a no-op.
func initAuth(ctx context.Context) error {
	loadAuthConfig()
	if !authEnabled() {
		log.Printf("auth: provider=none — chatbot runs unauthenticated")
		return nil
	}

	// When the bot reaches Keycloak at a different URL than the browser does
	// (the docker case — bot uses http://keycloak:8080, browser uses
	// http://localhost:8180), three things have to be rewired:
	//   1. Discovery URL ≠ issuer claim in metadata. Use InsecureIssuerURLContext
	//      so go-oidc accepts the mismatch.
	//   2. OAuth2 endpoint: browser hits AuthURL, bot hits TokenURL. They have
	//      to be different URLs.
	//   3. JWKS fetch from the bot must go via the compose-internal URL, not
	//      the localhost:8180 URL that's baked into Keycloak's metadata.
	// When DiscoveryURL == IssuerURL (the host-launched case) every URL is
	// the same and the metadata-derived endpoints are correct as-is.
	split := authCfg.DiscoveryURL != authCfg.IssuerURL
	discoveryCtx := ctx
	if split {
		discoveryCtx = oidc.InsecureIssuerURLContext(ctx, authCfg.IssuerURL)
	}
	provider, err := oidc.NewProvider(discoveryCtx, authCfg.DiscoveryURL)
	if err != nil {
		return fmt.Errorf("oidc discovery (%s): %w", authCfg.DiscoveryURL, err)
	}
	oidcProvider = provider

	endpoint := provider.Endpoint()
	if split {
		endpoint = oauth2.Endpoint{
			AuthURL:  authCfg.IssuerURL + "/protocol/openid-connect/auth",      // browser-facing
			TokenURL: authCfg.DiscoveryURL + "/protocol/openid-connect/token",  // bot-to-keycloak
		}
	}
	oauthConfig = oauth2.Config{
		ClientID:     authCfg.ClientID,
		ClientSecret: authCfg.ClientSecret,
		RedirectURL:  authCfg.RedirectURL,
		Endpoint:     endpoint,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
	}

	if split {
		// JWKS lives on Keycloak; bot must fetch it via the compose-internal URL.
		keySet := oidc.NewRemoteKeySet(ctx, authCfg.DiscoveryURL+"/protocol/openid-connect/certs")
		oidcVerifier = oidc.NewVerifier(authCfg.IssuerURL, keySet, &oidc.Config{ClientID: authCfg.ClientID})
	} else {
		oidcVerifier = provider.Verifier(&oidc.Config{ClientID: authCfg.ClientID})
	}
	authInitialized = true
	log.Printf("auth: provider=keycloak issuer=%s discovery=%s client=%s",
		authCfg.IssuerURL, authCfg.DiscoveryURL, authCfg.ClientID)

	// Janitor goroutine: prune pending-state map every minute.
	go func() {
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for range t.C {
			pendingStates.Range(func(k, v any) bool {
				if time.Since(v.(stateMeta).createdAt) > 10*time.Minute {
					pendingStates.Delete(k)
				}
				return true
			})
		}
	}()
	return nil
}

// ── User context helpers ───────────────────────────────────────────────────

type userCtxKey struct{}

func withUser(ctx context.Context, u *AuthenticatedUser) context.Context {
	return context.WithValue(ctx, userCtxKey{}, u)
}

func userFromCtx(ctx context.Context) *AuthenticatedUser {
	if v, ok := ctx.Value(userCtxKey{}).(*AuthenticatedUser); ok {
		return v
	}
	return nil
}

// ── Middleware ─────────────────────────────────────────────────────────────

// requireAuth wraps a handler so it returns 302 → /auth/login for
// unauthenticated requests, or proceeds with the user attached to context.
// If AUTH_PROVIDER=none, it's a pass-through.
func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !authEnabled() {
			next(w, r)
			return
		}

		user := sessionFromRequest(r)
		if user == nil {
			// HTMX requests can't follow a 302 cleanly — set HX-Redirect.
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/auth/login?return_to="+url.QueryEscape(r.URL.Path))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/auth/login?return_to="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
			return
		}

		maybeRefreshUserCache(r.Context(), user)

		ctx := withUser(r.Context(), user)
		ctx = WithCallContext(ctx, user.Subject, user.SessionID, "http")
		next(w, r.WithContext(ctx))
	}
}

func sessionFromRequest(r *http.Request) *AuthenticatedUser {
	c, err := r.Cookie("ontology_session")
	if err != nil {
		return nil
	}
	v, ok := sessionStore.Load(c.Value)
	if !ok {
		return nil
	}
	u := v.(*AuthenticatedUser)
	if time.Now().After(u.ExpiresAt) {
		sessionStore.Delete(c.Value)
		return nil
	}
	return u
}

// ── Handlers ───────────────────────────────────────────────────────────────

func authLoginHandler(w http.ResponseWriter, r *http.Request) {
	if !authEnabled() {
		http.Error(w, "auth disabled (set AUTH_PROVIDER=keycloak)", http.StatusServiceUnavailable)
		return
	}
	state := randomToken(32)
	returnTo := r.URL.Query().Get("return_to")
	if returnTo == "" {
		returnTo = "/"
	}
	pendingStates.Store(state, stateMeta{createdAt: time.Now(), returnTo: returnTo})
	http.Redirect(w, r, oauthConfig.AuthCodeURL(state), http.StatusFound)
}

func authCallbackHandler(w http.ResponseWriter, r *http.Request) {
	if !authEnabled() {
		http.Error(w, "auth disabled", http.StatusServiceUnavailable)
		return
	}
	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		http.Error(w, "keycloak error: "+errMsg+" — "+r.URL.Query().Get("error_description"), http.StatusBadRequest)
		return
	}
	state := r.URL.Query().Get("state")
	meta, ok := pendingStates.LoadAndDelete(state)
	if !ok {
		http.Error(w, "invalid or expired state", http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	oauth2Token, err := oauthConfig.Exchange(ctx, code)
	if err != nil {
		http.Error(w, "code exchange failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	rawIDToken, ok := oauth2Token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "no id_token in response", http.StatusBadRequest)
		return
	}
	idToken, err := oidcVerifier.Verify(ctx, rawIDToken)
	if err != nil {
		http.Error(w, "id_token verification failed: "+err.Error(), http.StatusUnauthorized)
		return
	}

	// Extract claims. realm_access.roles is what Keycloak uses by default.
	var claims struct {
		Sub               string `json:"sub"`
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
		RealmAccess       struct {
			Roles []string `json:"roles"`
		} `json:"realm_access"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "claims decode: "+err.Error(), http.StatusInternalServerError)
		return
	}

	sid := randomToken(32)
	user := &AuthenticatedUser{
		SessionID:    sid,
		Subject:      claims.Sub,
		Username:     claims.PreferredUsername,
		Email:        claims.Email,
		Name:         claims.Name,
		Roles:        claims.RealmAccess.Roles,
		AccessToken:  oauth2Token.AccessToken,
		IDToken:      rawIDToken,
		RefreshToken: oauth2Token.RefreshToken,
		ExpiresAt:    oauth2Token.Expiry,
	}
	user.cacheRefreshedAt = time.Now()
	sessionStore.Store(sid, user)
	upsertUserCache(r.Context(), user)

	http.SetCookie(w, &http.Cookie{
		Name:     "ontology_session",
		Value:    sid,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(time.Until(user.ExpiresAt).Seconds()),
	})

	rt := meta.(stateMeta).returnTo
	if rt == "" || !strings.HasPrefix(rt, "/") {
		rt = "/"
	}
	http.Redirect(w, r, rt, http.StatusFound)
}

func authLogoutHandler(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("ontology_session"); err == nil {
		sessionStore.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:    "ontology_session",
		Value:   "",
		Path:    "/",
		Expires: time.Unix(0, 0),
		MaxAge:  -1,
	})

	if !authEnabled() {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	// Bounce through Keycloak's end-session URL so the IdP-side session is
	// also cleared. The post-logout redirect_uri must match a configured
	// value in the client.
	endSession, _ := url.Parse(strings.TrimRight(authCfg.IssuerURL, "/") + "/protocol/openid-connect/logout")
	q := endSession.Query()
	q.Set("post_logout_redirect_uri", "http://localhost:8081/")
	q.Set("client_id", authCfg.ClientID)
	endSession.RawQuery = q.Encode()
	http.Redirect(w, r, endSession.String(), http.StatusFound)
}

// authMeHandler returns the current user as JSON — useful for the chat UI
// header to render "logged in as X / logout".
func authMeHandler(w http.ResponseWriter, r *http.Request) {
	user := sessionFromRequest(r)
	w.Header().Set("Content-Type", "application/json")
	if user == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"authenticated": false})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"authenticated": true,
		"username":      user.Username,
		"email":         user.Email,
		"name":          user.Name,
		"roles":         user.Roles,
	})
}

// ── Helpers ────────────────────────────────────────────────────────────────

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// extremely unlikely on real systems; fallback keeps the API alive.
		return base64.RawURLEncoding.EncodeToString([]byte(time.Now().String()))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// maybeRefreshUserCache writes the user's identity back to user_cache if
// it's been more than an hour since the last write. This catches role
// changes made in Keycloak after the session started without forcing a
// re-login. Lazy: noop if the cache was touched recently.
func maybeRefreshUserCache(ctx context.Context, u *AuthenticatedUser) {
	if u == nil || time.Since(u.cacheRefreshedAt) < time.Hour {
		return
	}
	u.cacheRefreshedAt = time.Now()
	upsertUserCache(ctx, u)
}

// upsertUserCache mirrors the latest user identity to the user_cache table
// so audit-log lookups can render display names without round-tripping
// Keycloak. Best-effort; failures are logged but don't block login.
func upsertUserCache(ctx context.Context, u *AuthenticatedUser) {
	if pgPool == nil {
		return
	}
	_, err := pgPool.Exec(ctx, `
        INSERT INTO user_cache (subject, email, name, roles)
        VALUES ($1::uuid, $2, $3, $4)
        ON CONFLICT (subject) DO UPDATE
        SET email = EXCLUDED.email,
            name  = EXCLUDED.name,
            roles = EXCLUDED.roles,
            updated_at = now()`,
		u.Subject, u.Email, u.Name, u.Roles)
	if err != nil {
		// Likely the user_cache table doesn't exist yet (migration 0008 not
		// applied) — log once, swallow. Auth still works.
		if !strings.Contains(err.Error(), "user_cache") {
			log.Printf("auth: user_cache upsert: %v", err)
		}
	}
}

// Sentinels so importers can detect missing config without runtime panics.
var (
	ErrAuthDisabled = errors.New("auth disabled (AUTH_PROVIDER!=keycloak)")
	_               = os.Setenv // silence unused-import warning if env not used
)
