package cloudserver

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	cloudauth "github.com/Gentleman-Programming/engram/internal/cloud/auth"
	"github.com/Gentleman-Programming/engram/internal/cloud/cloudstore"
	"github.com/Gentleman-Programming/engram/internal/cloud/constants"
)

const dashboardPrincipalSessionVersion = 1
const dashboardSessionKeyBytes = 32
const authAuditOutcomeDenied = "denied"
const authAuditActionDashboardBootstrap = "bootstrap.dashboard"

type principalStateStore interface {
	GetPrincipal(ctx context.Context, id string) (cloudstore.Principal, error)
}

type dashboardPrincipalStore interface {
	principalStateStore
	HasActiveAdmin(ctx context.Context) (bool, error)
	CreateHumanUser(ctx context.Context, params cloudstore.CreateHumanUserParams) (cloudstore.HumanUser, error)
	InsertAuthAuditEvent(ctx context.Context, event cloudstore.AuthAuditEvent) error
}

type dashboardPrincipalSessionClaims struct {
	PrincipalID     string `json:"principal_id"`
	PrincipalSource string `json:"principal_source"`
	Kind            string `json:"kind"`
	Role            string `json:"role"`
	DisplayName     string `json:"display_name"`
	IssuedAt        int64  `json:"iat"`
	ExpiresAt       int64  `json:"exp"`
	SessionVersion  int    `json:"session_version"`
	ManagedTokenID  string `json:"token_id,omitempty"`
}

func WithPrincipalStateStore(store principalStateStore) Option {
	return func(s *CloudServer) {
		s.principalState = store
	}
}

func (s *CloudServer) dashboardSessionTokenForRequest(ctx context.Context, bearerToken string) (string, error) {
	bearerToken = strings.TrimSpace(bearerToken)
	if bearerToken == "" {
		return "", fmt.Errorf("bearer token is required")
	}
	if s.principalAuth != nil {
		principal, err := s.principalAuth.ResolveBearerToken(ctx, bearerToken)
		if err == nil {
			if principal.Source == cloudauth.PrincipalSourceManagedToken {
				return s.mintDashboardPrincipalSession(principal)
			}
			return s.dashboardSessionToken(bearerToken)
		}
		if adminToken := strings.TrimSpace(s.dashboardAdmin); adminToken == "" || !hmac.Equal([]byte(bearerToken), []byte(adminToken)) {
			return "", err
		}
	}
	return s.dashboardSessionToken(bearerToken)
}

func (s *CloudServer) mintDashboardPrincipalSession(principal cloudauth.Principal) (string, error) {
	if err := principal.Validate(); err != nil {
		return "", err
	}
	if !principal.Enabled {
		return "", cloudauth.ErrPrincipalDisabled
	}
	issuedAt := time.Now().UTC()
	claims := dashboardPrincipalSessionClaims{
		PrincipalID:     strings.TrimSpace(principal.ID),
		PrincipalSource: string(principal.Source),
		Kind:            string(principal.Kind),
		Role:            string(principal.Role),
		DisplayName:     strings.TrimSpace(principal.DisplayName),
		IssuedAt:        issuedAt.Unix(),
		ExpiresAt:       issuedAt.Add(8 * time.Hour).Unix(),
		SessionVersion:  dashboardPrincipalSessionVersion,
		ManagedTokenID:  strings.TrimSpace(principal.TokenID),
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	payloadPart := base64.RawURLEncoding.EncodeToString(payload)
	signature, err := s.signDashboardPrincipalSession(payloadPart)
	if err != nil {
		return "", err
	}
	return payloadPart + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (s *CloudServer) parseDashboardPrincipalSession(sessionToken string) (cloudauth.Principal, error) {
	sessionToken = strings.TrimSpace(sessionToken)
	parts := strings.Split(sessionToken, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return cloudauth.Principal{}, cloudauth.ErrInvalidDashboardSessionToken
	}
	providedSig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return cloudauth.Principal{}, cloudauth.ErrInvalidDashboardSessionToken
	}
	expectedSig, err := s.signDashboardPrincipalSession(parts[0])
	if err != nil {
		return cloudauth.Principal{}, err
	}
	if !hmac.Equal(expectedSig, providedSig) {
		return cloudauth.Principal{}, cloudauth.ErrInvalidDashboardSessionToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return cloudauth.Principal{}, cloudauth.ErrInvalidDashboardSessionToken
	}
	var claims dashboardPrincipalSessionClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return cloudauth.Principal{}, cloudauth.ErrInvalidDashboardSessionToken
	}
	if claims.SessionVersion != dashboardPrincipalSessionVersion || claims.ExpiresAt <= time.Now().UTC().Unix() {
		return cloudauth.Principal{}, cloudauth.ErrInvalidDashboardSessionToken
	}
	principal := cloudauth.Principal{
		ID:          strings.TrimSpace(claims.PrincipalID),
		Kind:        cloudauth.PrincipalKind(strings.TrimSpace(claims.Kind)),
		DisplayName: strings.TrimSpace(claims.DisplayName),
		Role:        cloudauth.Role(strings.TrimSpace(claims.Role)),
		Enabled:     true,
		Source:      cloudauth.PrincipalSource(strings.TrimSpace(claims.PrincipalSource)),
		TokenID:     strings.TrimSpace(claims.ManagedTokenID),
	}
	if err := principal.Validate(); err != nil {
		return cloudauth.Principal{}, err
	}
	return principal, nil
}

func (s *CloudServer) dashboardPrincipalFromCookie(ctx context.Context, sessionToken string) (cloudauth.Principal, bool) {
	principal, err := s.parseDashboardPrincipalSession(sessionToken)
	if err != nil {
		return cloudauth.Principal{}, false
	}
	principal, err = s.revalidateDashboardPrincipal(ctx, principal)
	if err != nil {
		return cloudauth.Principal{}, false
	}
	return principal, true
}

func (s *CloudServer) dashboardPrincipalFromRequest(r *http.Request) (cloudauth.Principal, bool) {
	if principal, ok := PrincipalFromContext(r.Context()); ok {
		return principal, true
	}
	cookie, err := r.Cookie(dashboardSessionCookieName)
	if err != nil {
		return cloudauth.Principal{}, false
	}
	return s.dashboardPrincipalFromCookie(r.Context(), cookie.Value)
}

func (s *CloudServer) dashboardDisplayName(r *http.Request) string {
	if principal, ok := s.dashboardPrincipalFromRequest(r); ok && strings.TrimSpace(principal.DisplayName) != "" {
		return strings.TrimSpace(principal.DisplayName)
	}
	return "OPERATOR"
}

func (s *CloudServer) revalidateDashboardPrincipal(ctx context.Context, principal cloudauth.Principal) (cloudauth.Principal, error) {
	if principal.Source != cloudauth.PrincipalSourceManagedToken {
		if err := principal.Validate(); err != nil {
			return cloudauth.Principal{}, err
		}
		if !principal.Enabled {
			return cloudauth.Principal{}, cloudauth.ErrPrincipalDisabled
		}
		return principal, nil
	}
	if s.principalState == nil {
		return cloudauth.Principal{}, fmt.Errorf("dashboard principal state store is not configured")
	}
	current, err := s.principalState.GetPrincipal(ctx, principal.ID)
	if err != nil {
		return cloudauth.Principal{}, err
	}
	if !current.Enabled {
		return cloudauth.Principal{}, cloudauth.ErrPrincipalDisabled
	}
	revalidated := cloudauth.Principal{
		ID:          strings.TrimSpace(current.ID),
		Kind:        cloudauth.PrincipalKind(strings.TrimSpace(current.Kind)),
		DisplayName: strings.TrimSpace(current.DisplayName),
		Role:        cloudauth.Role(strings.TrimSpace(current.Role)),
		Enabled:     current.Enabled,
		Source:      cloudauth.PrincipalSourceManagedToken,
		TokenID:     strings.TrimSpace(principal.TokenID),
	}
	if err := revalidated.Validate(); err != nil {
		return cloudauth.Principal{}, err
	}
	return revalidated, nil
}

func (s *CloudServer) signDashboardPrincipalSession(payloadPart string) ([]byte, error) {
	key, err := s.dashboardPrincipalSessionKey()
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("engram-dashboard-principal-session:v1:"))
	_, _ = mac.Write([]byte(payloadPart))
	return mac.Sum(nil), nil
}

func (s *CloudServer) dashboardPrincipalSessionKey() ([]byte, error) {
	if len(s.dashboardSessionKey) >= dashboardSessionKeyBytes {
		return s.dashboardSessionKey, nil
	}
	key := make([]byte, dashboardSessionKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate dashboard session key: %w", err)
	}
	s.dashboardSessionKey = key
	return s.dashboardSessionKey, nil
}

func (s *CloudServer) handleDashboardBootstrapPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireLegacyDashboardRecovery(w, r); !ok {
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<html><body><main><h1>Create first managed admin</h1><form method="post" action="/dashboard/bootstrap"><label>Username <input name="username"></label><label>Email <input name="email"></label><label>Display name <input name="display_name"></label><button type="submit">Create admin</button></form></main></body></html>`))
}

func (s *CloudServer) handleDashboardBootstrapSubmit(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireLegacyDashboardRecovery(w, r)
	if !ok {
		return
	}
	store, ok := s.dashboardBootstrapStore(w)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		_ = s.recordDashboardBootstrapAudit(r.Context(), store, actor, authAuditOutcomeDenied, "invalid_form", "")
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	hasAdmin, err := store.HasActiveAdmin(r.Context())
	if err != nil {
		writeActionableError(w, http.StatusInternalServerError, constants.UpgradeErrorClassBlocked, constants.UpgradeErrorCodeInternal, fmt.Sprintf("check active admin: %v", err))
		return
	}
	if hasAdmin {
		_ = s.recordDashboardBootstrapAudit(r.Context(), store, actor, authAuditOutcomeDenied, "managed_admin_exists", "")
		writeActionableError(w, http.StatusConflict, constants.UpgradeErrorClassPolicy, constants.ReasonPolicyForbidden, "first managed admin already exists")
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	if username == "" {
		_ = s.recordDashboardBootstrapAudit(r.Context(), store, actor, authAuditOutcomeDenied, "username_required", "")
		writeActionableError(w, http.StatusBadRequest, constants.UpgradeErrorClassRepairable, constants.UpgradeErrorCodePayloadInvalid, "username is required")
		return
	}
	user, err := store.CreateHumanUser(r.Context(), cloudstore.CreateHumanUserParams{Username: username, Email: r.FormValue("email"), DisplayName: r.FormValue("display_name"), Role: cloudstore.PrincipalRoleAdmin})
	if err != nil {
		_ = s.recordDashboardBootstrapAudit(r.Context(), store, actor, authAuditOutcomeDenied, "create_admin_failed", "")
		writeActionableError(w, http.StatusBadRequest, constants.UpgradeErrorClassRepairable, constants.UpgradeErrorCodePayloadInvalid, fmt.Sprintf("create first admin: %v", err))
		return
	}
	if err := s.recordDashboardBootstrapAudit(r.Context(), store, actor, authAuditOutcomeSuccess, "", user.PrincipalID); err != nil {
		writeAuditFailure(w, err)
		return
	}
	http.Redirect(w, r, "/dashboard/login", http.StatusSeeOther)
}

func (s *CloudServer) requireLegacyDashboardRecovery(w http.ResponseWriter, r *http.Request) (cloudauth.Principal, bool) {
	if err := s.authorizeDashboardRequest(r); err != nil {
		http.Redirect(w, r, dashboardLoginPathWithNextLocal(r.URL.RequestURI()), http.StatusSeeOther)
		return cloudauth.Principal{}, false
	}
	principal, ok := PrincipalFromContext(r.Context())
	if !ok || principal.Source != cloudauth.PrincipalSourceLegacyEnvAdmin {
		writeActionableError(w, http.StatusForbidden, constants.UpgradeErrorClassPolicy, constants.ReasonPolicyForbidden, "forbidden: legacy dashboard recovery credential is required")
		return cloudauth.Principal{}, false
	}
	return principal, true
}

func (s *CloudServer) dashboardBootstrapStore(w http.ResponseWriter) (dashboardPrincipalStore, bool) {
	store, ok := s.adminIdentity.(dashboardPrincipalStore)
	if !ok || store == nil {
		writeActionableError(w, http.StatusInternalServerError, constants.UpgradeErrorClassBlocked, constants.UpgradeErrorCodeInternal, "dashboard bootstrap store is not configured")
		return nil, false
	}
	return store, true
}

func (s *CloudServer) recordDashboardBootstrapAudit(ctx context.Context, store dashboardPrincipalStore, actor cloudauth.Principal, outcome, reason, targetPrincipalID string) error {
	return store.InsertAuthAuditEvent(ctx, cloudstore.AuthAuditEvent{ActorPrincipalID: strings.TrimSpace(actor.ID), ActorSource: string(actor.Source), TargetPrincipalID: strings.TrimSpace(targetPrincipalID), Action: authAuditActionDashboardBootstrap, Outcome: strings.TrimSpace(outcome), ReasonCode: strings.TrimSpace(reason), Metadata: map[string]any{"source": string(actor.Source)}})
}

func dashboardLoginPathWithNextLocal(next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return "/dashboard/login"
	}
	return "/dashboard/login?next=" + url.QueryEscape(next)
}
