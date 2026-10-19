package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type registerInput struct{ Email, Password, Name, DateOfBirth, Phone, Organization string }
type loginInput struct{ Email, Password string }
type refreshInput struct {
	RefreshToken string `json:"refresh_token"`
}
type changePasswordInput struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}
type profileInput struct {
	Email     *string `json:"email"`
	Phone     *string `json:"phone"`
	AvatarURL *string `json:"avatarUrl"`
}
type elevationLocation struct {
	Latitude  float64 `json:"lat"`
	Longitude float64 `json:"lon"`
}
type elevationInput struct {
	Shape            []elevationLocation `json:"shape"`
	ResampleDistance float64             `json:"resample_distance"`
}
type billingRecord struct {
	UserID        string       `json:"userId"`
	Name          string       `json:"name"`
	Email         string       `json:"email"`
	Plan          string       `json:"plan"`
	PeriodStart   *time.Time   `json:"periodStart"`
	PeriodEnd     *time.Time   `json:"periodEnd"`
	LimitUnits    int          `json:"limitUnits"`
	UsedUnits     int          `json:"usedUnits"`
	DailyRequests []dailyUsage `json:"dailyRequests,omitempty"`
	APIRequests   []apiUsage   `json:"apiRequests,omitempty"`
}
type dailyUsage struct {
	Date     string `json:"date"`
	Requests int    `json:"requests"`
}
type apiUsage struct {
	API      string `json:"api"`
	Requests int    `json:"requests"`
}
type ageGroup struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}
type adminUserUpdateInput struct {
	Name       *string `json:"name"`
	Email      *string `json:"email"`
	Role       *string `json:"role"`
	Status     *string `json:"status"`
	Plan       *string `json:"plan"`
	LimitUnits *int    `json:"limitUnits"`
}
type adminUserCreateInput struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	Name         string `json:"name"`
	DateOfBirth  string `json:"dateOfBirth"`
	Phone        string `json:"phone"`
	Organization string `json:"organization"`
	Role         string `json:"role"`
	Status       string `json:"status"`
	Plan         string `json:"plan"`
	LimitUnits   int    `json:"limitUnits"`
}
type adminPasswordResetInput struct {
	NewPassword string `json:"newPassword"`
}

const defaultMonthlyQuotaLimit = 1000

func validDateOfBirth(value string) bool {
	date, err := time.Parse("2006-01-02", value)
	return err == nil && date.Before(time.Now().UTC().Truncate(24*time.Hour))
}

type Server struct {
	config          Config
	repository      *Repository
	passwords       PasswordService
	tokens          *TokenService
	revocations     *RevocationStore
	sessionEventHub *SessionEventHub
	loginLimiter    *RateLimiter
	loginSlots      chan struct{}
	dummyHash       string
	workerClient    *http.Client
}

func NewServer(config Config, repository *Repository, passwords PasswordService, tokens *TokenService, revocations *RevocationStore) (*Server, error) {
	dummyHash, err := passwords.Hash("not-a-real-password-37B2!")
	if err != nil {
		return nil, err
	}
	sessionEvents := NewSessionEventHub()
	revocations.SetOnApplied(sessionEvents.NotifyEvent)
	return &Server{config: config, repository: repository, passwords: passwords, tokens: tokens, revocations: revocations, sessionEventHub: sessionEvents, loginLimiter: NewRateLimiter(time.Duration(config.LoginWindowSeconds)*time.Second, config.LoginMaxAttempts), loginSlots: make(chan struct{}, config.LoginMaxConcurrent), dummyHash: dummyHash, workerClient: &http.Client{Timeout: 30 * time.Second}}, nil
}

func writeJSON(response http.ResponseWriter, status int, body any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}
func clientError(response http.ResponseWriter, status int, message string) {
	writeJSON(response, status, map[string]string{"error": message})
}
func unauthorized(response http.ResponseWriter) {
	clientError(response, http.StatusUnauthorized, "Unauthorized")
}

func decodeJSON(request *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(request.Body, 128*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("unexpected JSON values")
	}
	return nil
}

func normalizedEmail(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) == 0 || len(value) > 254 {
		return "", false
	}
	address, err := mail.ParseAddress(value)
	return value, err == nil && address.Address == value
}

func remoteIP(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil {
		return host
	}
	return request.RemoteAddr
}
func userAgent(request *http.Request) string {
	value := request.UserAgent()
	if len(value) > 500 {
		return value[:500]
	}
	return value
}

func hasScope(claims Claims, scope string) bool {
	for _, current := range claims.Scopes {
		if current == scope {
			return true
		}
	}
	return false
}

func isAdmin(claims Claims) bool { return claims.Role == "admin" }

func (server *Server) authenticate(response http.ResponseWriter, request *http.Request, scopes ...string) (Claims, bool) {
	if !server.revocations.Ready() {
		clientError(response, http.StatusServiceUnavailable, "Authentication temporarily unavailable")
		return Claims{}, false
	}
	claims, err := server.tokens.Verify(bearerToken(request.Header.Get("Authorization")))
	if err != nil || server.revocations.Rejects(claims) {
		unauthorized(response)
		return Claims{}, false
	}
	active, err := server.repository.TouchActiveSession(request.Context(), server.repository.pool, claims.SessionID, server.config.SessionIdleTimeoutSeconds)
	if err != nil {
		clientError(response, http.StatusServiceUnavailable, "Authentication temporarily unavailable")
		return Claims{}, false
	}
	if !active {
		unauthorized(response)
		return Claims{}, false
	}
	if claims.MustChangePassword && !(request.Method == http.MethodPost && (request.URL.Path == "/auth/change-password" || request.URL.Path == "/auth/logout")) {
		writeJSON(response, http.StatusForbidden, map[string]string{"error": "Password change required", "code": "passwordChangeRequired"})
		return Claims{}, false
	}
	for _, permission := range scopes {
		if !hasPermission(claims, permission) {
			clientError(response, http.StatusForbidden, "Forbidden")
			return Claims{}, false
		}
	}
	return claims, true
}

func cookieOptions(server *Server, maxAge int) *http.Cookie {
	return &http.Cookie{Name: "refresh_token", HttpOnly: true, Secure: server.config.SecureCookies, SameSite: http.SameSiteStrictMode, Path: "/auth", MaxAge: maxAge}
}
func (server *Server) setRefreshCookies(response http.ResponseWriter, token string) {
	cookie := cookieOptions(server, server.config.RefreshDays*86400)
	cookie.Value = token
	http.SetCookie(response, cookie)
	http.SetCookie(response, &http.Cookie{Name: "csrf_token", Value: newRequestID(), Secure: server.config.SecureCookies, SameSite: http.SameSiteStrictMode, Path: "/", MaxAge: server.config.RefreshDays * 86400})
}
func (server *Server) clearRefreshCookies(response http.ResponseWriter) {
	http.SetCookie(response, cookieOptions(server, -1))
	http.SetCookie(response, &http.Cookie{Name: "csrf_token", Value: "", Secure: server.config.SecureCookies, SameSite: http.SameSiteStrictMode, Path: "/", MaxAge: -1})
}
func newRequestID() string {
	value, err := randomURLToken(16)
	if err != nil {
		return "unavailable"
	}
	return value
}

func csrfValid(request *http.Request) bool {
	cookie, err := request.Cookie("csrf_token")
	if err != nil {
		return false
	}
	header := request.Header.Get("X-CSRF-Token")
	return header != "" && len(cookie.Value) == len(header) && subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(header)) == 1
}

func (server *Server) issueLoginResponse(response http.ResponseWriter, user User, sessionID, refreshToken string) {
	access, expiresIn, err := server.tokens.Issue(user, sessionID)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	server.setRefreshCookies(response, refreshToken)
	writeJSON(response, 200, map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": expiresIn, "idle_timeout_seconds": server.config.SessionIdleTimeoutSeconds, "user": publicUser(user)})
}

func (server *Server) register(response http.ResponseWriter, request *http.Request) {
	var input registerInput
	if decodeJSON(request, &input) != nil {
		clientError(response, 400, "Invalid registration request")
		return
	}
	email, valid := normalizedEmail(input.Email)
	if !valid || validatePassword(input.Password) != "" || len(strings.TrimSpace(input.Name)) > 120 || len(input.Phone) > 32 || len(input.Organization) > 160 {
		clientError(response, 400, "Invalid registration request")
		return
	}
	if input.DateOfBirth != "" {
		if _, err := time.Parse("2006-01-02", input.DateOfBirth); err != nil {
			clientError(response, 400, "Invalid registration request")
			return
		}
	}
	hash, err := server.passwords.Hash(input.Password)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	ctx := request.Context()
	tx, err := server.repository.pool.Begin(ctx)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	defer tx.Rollback(ctx)
	if _, err := server.repository.UserByEmail(ctx, tx, email); err == nil {
		clientError(response, 409, "Email is already registered")
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		clientError(response, 500, "Internal server error")
		return
	}
	input.Email = email
	input.Name = strings.TrimSpace(input.Name)
	user, err := server.repository.CreateUser(ctx, tx, email, hash, input)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	_, err = tx.Exec(ctx, `INSERT INTO user_quotas (user_id,period_start,period_end,limit_units) VALUES ($1,date_trunc('month',now()),date_trunc('month',now()) + interval '1 month',$2)`, user.ID, defaultMonthlyQuotaLimit)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	writeJSON(response, 201, map[string]any{"user": publicUser(user)})
}

func (server *Server) login(response http.ResponseWriter, request *http.Request) {
	var input loginInput
	if decodeJSON(request, &input) != nil {
		clientError(response, 400, "Invalid credentials")
		return
	}
	email, valid := normalizedEmail(input.Email)
	if !valid || len(input.Password) == 0 || len(input.Password) > 128 {
		clientError(response, 400, "Invalid credentials")
		return
	}
	if !server.loginLimiter.Allow("ip:"+remoteIP(request)) || !server.loginLimiter.Allow("identity:"+email) {
		clientError(response, 429, "Too many attempts. Try again later.")
		return
	}
	select {
	case server.loginSlots <- struct{}{}:
		defer func() { <-server.loginSlots }()
	case <-request.Context().Done():
		clientError(response, 503, "Authentication temporarily unavailable")
		return
	}
	ctx := request.Context()
	user, err := server.repository.UserByEmail(ctx, server.repository.pool, email)
	hash := server.dummyHash
	if err == nil {
		hash = user.PasswordHash
	}
	validPassword := server.passwords.Verify(hash, input.Password)
	if err != nil || !validPassword {
		clientError(response, 401, "Invalid credentials")
		return
	}
	switch user.Status {
	case "DISABLED":
		writeJSON(response, http.StatusForbidden, map[string]string{"error": "Account is disabled", "code": "accountDisabled"})
		return
	case "LOCKED":
		writeJSON(response, http.StatusLocked, map[string]string{"error": "Account is locked", "code": "accountLocked"})
		return
	case "ACTIVE":
		// The account may continue with session creation below.
	default:
		clientError(response, 401, "Invalid credentials")
		return
	}
	if server.passwords.NeedsRehash(user.PasswordHash) {
		if upgraded, hashErr := server.passwords.Hash(input.Password); hashErr == nil {
			_, _ = server.repository.pool.Exec(ctx, `UPDATE users SET password_hash=$1 WHERE id=$2`, upgraded, user.ID)
		}
	}
	refresh, err := server.tokens.CreateRefreshToken()
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	tx, err := server.repository.pool.Begin(ctx)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	defer tx.Rollback(ctx)
	session, err := server.repository.CreateSession(ctx, tx, user.ID, server.tokens.HashRefreshToken(refresh), userAgent(request), remoteIP(request), time.Now().AddDate(0, 0, server.config.RefreshDays))
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET last_login_at=now() WHERE id=$1`, user.ID); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	slog.Info("login_success", "user_id", user.ID, "request_id", request.Header.Get("X-Request-Id"))
	server.issueLoginResponse(response, user, session.ID, refresh)
}

func (server *Server) refresh(response http.ResponseWriter, request *http.Request) {
	var input refreshInput
	_ = decodeJSON(request, &input)
	token := input.RefreshToken
	fromCookie := false
	if token == "" {
		if cookie, err := request.Cookie("refresh_token"); err == nil {
			token = cookie.Value
			fromCookie = true
		}
	}
	if token == "" || (fromCookie && !csrfValid(request)) {
		unauthorized(response)
		return
	}
	ctx := request.Context()
	hash := server.tokens.HashRefreshToken(token)
	tx, err := server.repository.pool.Begin(ctx)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	defer tx.Rollback(ctx)
	session, sessionErr := server.repository.SessionForRefreshWithinIdleTimeout(ctx, tx, hash, server.config.SessionIdleTimeoutSeconds)
	if sessionErr != nil {
		sid, uid, historyErr := server.repository.RefreshHistory(ctx, tx, hash)
		if historyErr == nil {
			_, _ = tx.Exec(ctx, `UPDATE sessions SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1`, sid)
			event := RevocationEvent{Type: "SessionRevoked", SessionID: sid, UserID: uid, Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}
			if err := server.repository.Publish(ctx, tx, event); err == nil {
				server.revocations.Apply(event)
			}
		}
		_ = tx.Commit(ctx)
		server.clearRefreshCookies(response)
		unauthorized(response)
		return
	}
	if !matchesRefreshToken(session.RefreshTokenHash, hash) || session.RevokedAt != nil || !session.ExpiresAt.After(time.Now()) {
		server.clearRefreshCookies(response)
		unauthorized(response)
		return
	}
	user, err := server.repository.UserByID(ctx, tx, session.UserID)
	if err != nil || user.Status != "ACTIVE" {
		server.clearRefreshCookies(response)
		unauthorized(response)
		return
	}
	next, err := server.tokens.CreateRefreshToken()
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	if err = server.repository.RotateRefresh(ctx, tx, session, server.tokens.HashRefreshToken(next)); err != nil {
		clientError(response, 401, "Unauthorized")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	server.issueLoginResponse(response, user, session.ID, next)
}

// sessionEvents is a server-to-browser event stream. It authenticates with the
// existing HttpOnly refresh cookie (scoped to /auth), so no access token is put
// into the URL or exposed to JavaScript.
func (server *Server) sessionEvents(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		clientError(response, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	cookie, err := request.Cookie("refresh_token")
	if err != nil || cookie.Value == "" {
		unauthorized(response)
		return
	}
	ctx := request.Context()
	session, err := server.repository.SessionForRefreshWithinIdleTimeout(ctx, server.repository.pool, server.tokens.HashRefreshToken(cookie.Value), server.config.SessionIdleTimeoutSeconds)
	if err != nil {
		unauthorized(response)
		return
	}
	user, err := server.repository.UserByID(ctx, server.repository.pool, session.UserID)
	if err != nil || user.Status != "ACTIVE" {
		unauthorized(response)
		return
	}
	flusher, ok := response.(http.Flusher)
	if !ok {
		clientError(response, http.StatusInternalServerError, "Streaming unavailable")
		return
	}
	writeDeadline := http.NewResponseController(response)
	refreshWriteDeadline := func() bool {
		return writeDeadline.SetWriteDeadline(time.Now().Add(30*time.Second)) == nil
	}

	response.Header().Set("Cache-Control", "no-cache, no-transform")
	response.Header().Set("Connection", "keep-alive")
	response.Header().Set("Content-Type", "text/event-stream")
	response.Header().Set("X-Accel-Buffering", "no")
	updates, unsubscribe := server.sessionEventHub.Subscribe(user.ID)
	defer unsubscribe()
	if !refreshWriteDeadline() {
		return
	}
	if _, err := response.Write([]byte("event: ready\ndata: {}\n\n")); err != nil {
		return
	}
	flusher.Flush()

	keepAlive := time.NewTicker(25 * time.Second)
	defer keepAlive.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case update := <-updates:
			if !refreshWriteDeadline() {
				return
			}
			if err := writeSSE(response, update); err != nil {
				return
			}
			flusher.Flush()
			if update.Type == "session-updated" {
				return
			}
		case <-keepAlive.C:
			if !refreshWriteDeadline() {
				return
			}
			if _, err := response.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (server *Server) logout(response http.ResponseWriter, request *http.Request, all bool) {
	claims, ok := server.authenticate(response, request)
	if !ok {
		server.clearRefreshCookies(response)
		return
	}
	ctx := request.Context()
	tx, err := server.repository.pool.Begin(ctx)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	defer tx.Rollback(ctx)
	if all {
		if _, err = tx.Exec(ctx, `UPDATE sessions SET revoked_at=COALESCE(revoked_at,now()) WHERE user_id=$1 AND revoked_at IS NULL`, claims.Subject); err != nil {
			clientError(response, 500, "Internal server error")
			return
		}
		var version int
		if err = tx.QueryRow(ctx, `UPDATE users SET auth_version=auth_version+1 WHERE id=$1 RETURNING auth_version`, claims.Subject).Scan(&version); err != nil {
			clientError(response, 500, "Internal server error")
			return
		}
		event := RevocationEvent{Type: "UserSessionsRevoked", UserID: claims.Subject, AuthVersion: version, Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}
		if err = server.repository.Publish(ctx, tx, event); err != nil {
			clientError(response, 500, "Internal server error")
			return
		}
		server.revocations.Apply(event)
	} else {
		var userID string
		err = tx.QueryRow(ctx, `UPDATE sessions SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1 RETURNING user_id`, claims.SessionID).Scan(&userID)
		if err == nil {
			event := RevocationEvent{Type: "SessionRevoked", SessionID: claims.SessionID, UserID: userID, Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}
			if err = server.repository.Publish(ctx, tx, event); err != nil {
				clientError(response, 500, "Internal server error")
				return
			}
			server.revocations.Apply(event)
		} else if !errors.Is(err, pgx.ErrNoRows) {
			clientError(response, 500, "Internal server error")
			return
		}
	}
	if err = tx.Commit(ctx); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	server.clearRefreshCookies(response)
	response.WriteHeader(http.StatusNoContent)
}

func (server *Server) changePassword(response http.ResponseWriter, request *http.Request) {
	claims, ok := server.authenticate(response, request)
	if !ok {
		return
	}
	var input changePasswordInput
	if decodeJSON(request, &input) != nil || validatePassword(input.NewPassword) != "" {
		clientError(response, 400, "Invalid password change request")
		return
	}
	if input.NewPassword == input.CurrentPassword {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "Choose a different password", "code": "passwordMustDiffer"})
		return
	}
	ctx := request.Context()
	user, err := server.repository.UserByID(ctx, server.repository.pool, claims.Subject)
	if err != nil || user.Status != "ACTIVE" || !server.passwords.Verify(user.PasswordHash, input.CurrentPassword) {
		clientError(response, 401, "Invalid credentials")
		return
	}
	nextHash, err := server.passwords.Hash(input.NewPassword)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	tx, err := server.repository.pool.Begin(ctx)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE sessions SET revoked_at=COALESCE(revoked_at,now()) WHERE user_id=$1 AND revoked_at IS NULL`, user.ID); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	var version int
	if err = tx.QueryRow(ctx, `UPDATE users SET password_hash=$1,must_change_password=false,auth_version=auth_version+1 WHERE id=$2 RETURNING auth_version`, nextHash, user.ID).Scan(&version); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	event := RevocationEvent{Type: "PasswordChanged", UserID: user.ID, AuthVersion: version, Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = server.repository.Publish(ctx, tx, event); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	server.revocations.Apply(event)
	server.clearRefreshCookies(response)
	slog.Info("password_changed", "user_id", user.ID)
	response.WriteHeader(http.StatusNoContent)
}

func (server *Server) profile(response http.ResponseWriter, request *http.Request, update bool) {
	permission := permissionAccountRead
	if update {
		permission = permissionAccountWrite
	}
	claims, ok := server.authenticate(response, request, permission)
	if !ok {
		return
	}
	ctx := request.Context()
	if !update {
		user, err := server.repository.UserByID(ctx, server.repository.pool, claims.Subject)
		if err != nil || user.Status != "ACTIVE" {
			unauthorized(response)
			return
		}
		writeJSON(response, 200, map[string]any{"user": publicUser(user)})
		return
	}
	var input profileInput
	if decodeJSON(request, &input) != nil || (input.Email == nil && input.Phone == nil && input.AvatarURL == nil) {
		clientError(response, 400, "Invalid profile update")
		return
	}
	if input.Email != nil {
		email, valid := normalizedEmail(*input.Email)
		if !valid {
			clientError(response, 400, "Invalid profile update")
			return
		}
		input.Email = &email
	}
	if input.Phone != nil && len(*input.Phone) > 32 || input.AvatarURL != nil && len(*input.AvatarURL) > 1_000_000 {
		clientError(response, 400, "Invalid profile update")
		return
	}
	user, err := scanUser(server.repository.pool.QueryRow(ctx, `UPDATE users SET email=COALESCE($1,email),phone=COALESCE($2,phone),avatar_url=COALESCE($3,avatar_url) WHERE id=$4 RETURNING `+userColumns, input.Email, input.Phone, input.AvatarURL, claims.Subject))
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			clientError(response, 409, "Email is already registered")
		} else {
			clientError(response, 500, "Internal server error")
		}
		return
	}
	writeJSON(response, 200, map[string]any{"user": publicUser(user)})
}

func billingDays(request *http.Request) int {
	switch request.URL.Query().Get("range") {
	case "1":
		return 1
	case "30":
		return 30
	default:
		return 7
	}
}

// ensureCurrentQuotas lazily rolls an expired calendar-month quota into the
// current month. Existing limits are retained while usage starts at zero. An
// empty userID updates every account for administrator list views.
func (server *Server) ensureCurrentQuotas(ctx context.Context, userID string) error {
	_, err := server.repository.pool.Exec(ctx, `
		INSERT INTO user_quotas (user_id, period_start, period_end, limit_units, used_units)
		SELECT u.id, date_trunc('month', now()), date_trunc('month', now()) + interval '1 month',
			COALESCE(q.limit_units, $2), 0
		FROM users u
		LEFT JOIN user_quotas q ON q.user_id = u.id
		WHERE (NULLIF($1, '')::uuid IS NULL OR u.id = NULLIF($1, '')::uuid)
			AND (q.user_id IS NULL OR q.period_start > now() OR q.period_end <= now())
		ON CONFLICT (user_id) DO UPDATE
		SET period_start = EXCLUDED.period_start,
			period_end = EXCLUDED.period_end,
			limit_units = EXCLUDED.limit_units,
			used_units = 0,
			updated_at = now()
		WHERE user_quotas.period_start > now() OR user_quotas.period_end <= now()`, userID, defaultMonthlyQuotaLimit)
	return err
}

func (server *Server) consumeQuota(ctx context.Context, userID, apiType string) error {
	result, err := server.repository.pool.Exec(ctx, `
		WITH consumed_quota AS (
			INSERT INTO user_quotas (user_id, period_start, period_end, limit_units, used_units)
			VALUES ($1, date_trunc('month', now()), date_trunc('month', now()) + interval '1 month', $3, 1)
			ON CONFLICT (user_id) DO UPDATE
			SET period_start = CASE WHEN NOT (user_quotas.period_start <= now() AND user_quotas.period_end > now()) THEN EXCLUDED.period_start ELSE user_quotas.period_start END,
				period_end = CASE WHEN NOT (user_quotas.period_start <= now() AND user_quotas.period_end > now()) THEN EXCLUDED.period_end ELSE user_quotas.period_end END,
				used_units = CASE WHEN NOT (user_quotas.period_start <= now() AND user_quotas.period_end > now()) THEN 1 ELSE user_quotas.used_units + 1 END,
				updated_at = now()
			WHERE user_quotas.limit_units > 0
				AND (NOT (user_quotas.period_start <= now() AND user_quotas.period_end > now()) OR user_quotas.used_units < user_quotas.limit_units)
			RETURNING user_id
		)
		INSERT INTO user_daily_usage (user_id, usage_date, api_type, request_count)
		SELECT user_id, current_date, $2, 1 FROM consumed_quota
		ON CONFLICT (user_id, usage_date, api_type) DO UPDATE
		SET request_count = user_daily_usage.request_count + 1`, userID, apiType, defaultMonthlyQuotaLimit)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return errors.New("quota exceeded")
	}
	return nil
}

func (server *Server) billingForUser(ctx context.Context, userID string, days int) (billingRecord, error) {
	var billing billingRecord
	if err := server.ensureCurrentQuotas(ctx, userID); err != nil {
		return billing, err
	}
	err := server.repository.pool.QueryRow(ctx, `
		SELECT u.id, COALESCE(u.full_name, u.email), u.email, u.plan,
			q.period_start, q.period_end, COALESCE(q.limit_units, 0), COALESCE(q.used_units, 0)
		FROM users u
		LEFT JOIN LATERAL (
			SELECT period_start, period_end, limit_units, used_units
			FROM user_quotas
			WHERE user_id = u.id AND period_start <= now() AND period_end > now()
			ORDER BY period_end DESC
			LIMIT 1
		) q ON true
		WHERE u.id = $1`, userID).Scan(
		&billing.UserID, &billing.Name, &billing.Email, &billing.Plan,
		&billing.PeriodStart, &billing.PeriodEnd, &billing.LimitUnits, &billing.UsedUnits,
	)
	if err != nil {
		return billing, err
	}
	rows, err := server.repository.pool.Query(ctx, `
		SELECT day::date::text, COALESCE(usage.request_count, 0)
		FROM generate_series(current_date - ($2::integer - 1), current_date, interval '1 day') AS days(day)
		LEFT JOIN (
			SELECT usage_date, SUM(request_count) AS request_count
			FROM user_daily_usage WHERE user_id = $1 AND api_type IN ('route', 'search') GROUP BY usage_date
		) usage ON usage.usage_date = days.day::date
		ORDER BY days.day`, userID, days)
	if err != nil {
		return billing, err
	}
	defer rows.Close()
	billing.DailyRequests = make([]dailyUsage, 0, days)
	for rows.Next() {
		var usage dailyUsage
		if err := rows.Scan(&usage.Date, &usage.Requests); err != nil {
			return billing, err
		}
		billing.DailyRequests = append(billing.DailyRequests, usage)
	}
	if err := rows.Err(); err != nil {
		return billing, err
	}
	apiRows, err := server.repository.pool.Query(ctx, `
		SELECT api_type, SUM(request_count)
		FROM user_daily_usage
		WHERE user_id=$1 AND usage_date >= current_date - ($2::integer - 1) AND api_type IN ('route', 'search')
		GROUP BY api_type ORDER BY api_type`, userID, days)
	if err != nil {
		return billing, err
	}
	defer apiRows.Close()
	billing.APIRequests = make([]apiUsage, 0, 2)
	for apiRows.Next() {
		var usage apiUsage
		if err := apiRows.Scan(&usage.API, &usage.Requests); err != nil {
			return billing, err
		}
		billing.APIRequests = append(billing.APIRequests, usage)
	}
	return billing, apiRows.Err()
}

// billing only returns the signed-in user's subscription, quota, and requests
// counted in the selected reporting window, grouped by API type.
func (server *Server) billing(response http.ResponseWriter, request *http.Request) {
	claims, ok := server.authenticate(response, request, permissionBillingRead)
	if !ok {
		return
	}
	billing, err := server.billingForUser(request.Context(), claims.Subject, billingDays(request))
	if errors.Is(err, pgx.ErrNoRows) {
		unauthorized(response)
		return
	}
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	writeJSON(response, 200, billing)
}

func (server *Server) adminDashboard(response http.ResponseWriter, request *http.Request) {
	if _, ok := server.requireAdmin(response, request); !ok {
		return
	}
	var totalAccounts, onlineUsers, unknownAge, under18, age18To24, age25To34, age35To44, age45Plus int
	err := server.repository.pool.QueryRow(request.Context(), `
		SELECT
			COUNT(DISTINCT u.id),
			COUNT(DISTINCT s.user_id) FILTER (WHERE s.last_used_at > now() - interval '5 minutes' AND s.revoked_at IS NULL AND s.expires_at > now()),
			COUNT(DISTINCT u.id) FILTER (WHERE u.date_of_birth IS NULL),
			COUNT(DISTINCT u.id) FILTER (WHERE u.date_of_birth > current_date - interval '18 years'),
			COUNT(DISTINCT u.id) FILTER (WHERE u.date_of_birth <= current_date - interval '18 years' AND u.date_of_birth > current_date - interval '25 years'),
			COUNT(DISTINCT u.id) FILTER (WHERE u.date_of_birth <= current_date - interval '25 years' AND u.date_of_birth > current_date - interval '35 years'),
			COUNT(DISTINCT u.id) FILTER (WHERE u.date_of_birth <= current_date - interval '35 years' AND u.date_of_birth > current_date - interval '45 years'),
			COUNT(DISTINCT u.id) FILTER (WHERE u.date_of_birth <= current_date - interval '45 years')
		FROM users u
		LEFT JOIN sessions s ON s.user_id = u.id`).Scan(
		&totalAccounts, &onlineUsers, &unknownAge, &under18, &age18To24, &age25To34, &age35To44, &age45Plus,
	)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	organizationRows, err := server.repository.pool.Query(request.Context(), `
		SELECT COALESCE(NULLIF(BTRIM(organization), ''), 'Chưa cập nhật'), COUNT(*)
		FROM users
		GROUP BY 1
		ORDER BY COUNT(*) DESC, 1
		LIMIT 8`)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	defer organizationRows.Close()
	organizationGroups := make([]ageGroup, 0)
	for organizationRows.Next() {
		var group ageGroup
		if err := organizationRows.Scan(&group.Label, &group.Count); err != nil {
			clientError(response, 500, "Internal server error")
			return
		}
		organizationGroups = append(organizationGroups, group)
	}
	if err := organizationRows.Err(); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	writeJSON(response, 200, map[string]any{
		"totalAccounts": totalAccounts,
		"onlineUsers":   onlineUsers,
		"ageGroups": []ageGroup{
			{Label: "<18", Count: under18},
			{Label: "18–24", Count: age18To24},
			{Label: "25–34", Count: age25To34},
			{Label: "35–44", Count: age35To44},
			{Label: "45+", Count: age45Plus},
			{Label: "Unknown", Count: unknownAge},
		},
		"organizationGroups": organizationGroups,
	})
}

func (server *Server) adminBilling(response http.ResponseWriter, request *http.Request) {
	if _, ok := server.requireAdmin(response, request); !ok {
		return
	}
	if err := server.ensureCurrentQuotas(request.Context(), ""); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	rows, err := server.repository.pool.Query(request.Context(), `
		SELECT u.id, COALESCE(u.full_name, u.email), u.email, u.plan,
			q.period_start, q.period_end, COALESCE(q.limit_units, 0), COALESCE(q.used_units, 0)
		FROM users u
		LEFT JOIN LATERAL (
			SELECT period_start, period_end, limit_units, used_units
			FROM user_quotas
			WHERE user_id = u.id AND period_start <= now() AND period_end > now()
			ORDER BY period_end DESC
			LIMIT 1
		) q ON true
		ORDER BY u.created_at DESC`)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	defer rows.Close()
	billings := make([]billingRecord, 0)
	for rows.Next() {
		var billing billingRecord
		if err := rows.Scan(&billing.UserID, &billing.Name, &billing.Email, &billing.Plan, &billing.PeriodStart, &billing.PeriodEnd, &billing.LimitUnits, &billing.UsedUnits); err != nil {
			clientError(response, 500, "Internal server error")
			return
		}
		billings = append(billings, billing)
	}
	if rows.Err() != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	writeJSON(response, 200, map[string]any{"billings": billings})
}

// adminBillingUser is deliberately separate from the user's own billing route:
// an administrator may inspect a selected account, while ordinary users can
// only request the billing record for their token subject.
func (server *Server) adminBillingUser(response http.ResponseWriter, request *http.Request, userID string) {
	if _, ok := server.requireAdmin(response, request); !ok {
		return
	}
	billing, err := server.billingForUser(request.Context(), userID, billingDays(request))
	if errors.Is(err, pgx.ErrNoRows) {
		clientError(response, http.StatusNotFound, "User not found")
		return
	}
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	writeJSON(response, 200, billing)
}

func (server *Server) requireAdmin(response http.ResponseWriter, request *http.Request) (Claims, bool) {
	claims, ok := server.authenticate(response, request, permissionAdminManage)
	if !ok || isAdmin(claims) {
		return claims, ok
	}
	clientError(response, http.StatusForbidden, "Forbidden")
	return Claims{}, false
}

func (server *Server) adminUsers(response http.ResponseWriter, request *http.Request) {
	if _, ok := server.requireAdmin(response, request); !ok {
		return
	}
	if err := server.ensureCurrentQuotas(request.Context(), ""); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	rows, err := server.repository.pool.Query(request.Context(), `SELECT u.id,COALESCE(u.full_name,u.email),u.email,u.role,u.status,u.plan,COALESCE(q.limit_units,0),COALESCE(q.used_units,0),EXISTS(SELECT 1 FROM sessions s WHERE s.user_id=u.id AND s.revoked_at IS NULL AND s.expires_at>now() AND s.last_used_at>now()-interval '5 minutes') FROM users u LEFT JOIN LATERAL (SELECT limit_units,used_units FROM user_quotas WHERE user_id=u.id AND period_start<=now() AND period_end>now() LIMIT 1) q ON true ORDER BY u.created_at DESC`)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	defer rows.Close()
	users := make([]map[string]any, 0)
	for rows.Next() {
		var id, name, email, role, status, plan string
		var limit, used int
		var online bool
		if err := rows.Scan(&id, &name, &email, &role, &status, &plan, &limit, &used, &online); err != nil {
			clientError(response, 500, "Internal server error")
			return
		}
		users = append(users, map[string]any{"id": id, "name": name, "email": email, "role": role, "status": status, "plan": plan, "limitUnits": limit, "usedUnits": used, "online": online})
	}
	if rows.Err() != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	writeJSON(response, 200, map[string]any{"users": users})
}

func (server *Server) adminCreateUser(response http.ResponseWriter, request *http.Request) {
	claims, ok := server.requireAdmin(response, request)
	if !ok {
		return
	}
	var input adminUserCreateInput
	if decodeJSON(request, &input) != nil {
		clientError(response, 400, "Invalid user")
		return
	}
	email, valid := normalizedEmail(input.Email)
	input.Name = strings.TrimSpace(input.Name)
	input.Plan = strings.TrimSpace(input.Plan)
	if !valid || validatePassword(input.Password) != "" || len(input.Name) > 120 || len(input.Phone) > 32 || len(input.Organization) > 160 || (input.DateOfBirth != "" && !validDateOfBirth(input.DateOfBirth)) || (input.Role != "user" && input.Role != "admin") || (input.Status != "ACTIVE" && input.Status != "DISABLED" && input.Status != "LOCKED") || input.Plan == "" || len(input.Plan) > 64 || input.LimitUnits < 0 || input.LimitUnits > 10_000_000 {
		clientError(response, 400, "Invalid user")
		return
	}
	hash, err := server.passwords.Hash(input.Password)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	ctx := request.Context()
	tx, err := server.repository.pool.Begin(ctx)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	defer tx.Rollback(ctx)
	if _, err = server.repository.UserByEmail(ctx, tx, email); err == nil {
		clientError(response, 409, "Email is already registered")
		return
	} else if !errors.Is(err, pgx.ErrNoRows) {
		clientError(response, 500, "Internal server error")
		return
	}
	registration := registerInput{Email: email, Password: input.Password, Name: input.Name, DateOfBirth: input.DateOfBirth, Phone: input.Phone, Organization: input.Organization}
	user, err := server.repository.CreateUser(ctx, tx, email, hash, registration)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE users SET role=$1,status=$2,plan=$3 WHERE id=$4`, input.Role, input.Status, input.Plan, user.ID); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	if _, err = tx.Exec(ctx, `INSERT INTO user_quotas(user_id,period_start,period_end,limit_units) VALUES($1,date_trunc('month',now()),date_trunc('month',now()) + interval '1 month',$2)`, user.ID, input.LimitUnits); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	details, _ := json.Marshal(map[string]any{"created": map[string]any{"name": input.Name, "email": email, "role": input.Role, "status": input.Status, "plan": input.Plan, "limitUnits": input.LimitUnits}})
	if _, err = tx.Exec(ctx, `INSERT INTO admin_audit_logs(actor_id,target_user_id,action,details) VALUES($1,$2,'user.created',$3::jsonb)`, claims.Subject, user.ID, details); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	writeJSON(response, http.StatusCreated, map[string]any{"id": user.ID})
}

func (server *Server) adminUpdateUser(response http.ResponseWriter, request *http.Request, userID string) {
	claims, ok := server.requireAdmin(response, request)
	if !ok {
		return
	}
	var input adminUserUpdateInput
	if decodeJSON(request, &input) != nil || input.Name == nil && input.Email == nil && input.Role == nil && input.Status == nil && input.Plan == nil && input.LimitUnits == nil {
		clientError(response, 400, "Invalid user update")
		return
	}
	if input.Role != nil && *input.Role != "user" && *input.Role != "admin" {
		clientError(response, 400, "Invalid user update")
		return
	}
	if input.Status != nil && *input.Status != "ACTIVE" && *input.Status != "DISABLED" && *input.Status != "LOCKED" {
		clientError(response, 400, "Invalid user update")
		return
	}
	if input.Plan != nil {
		value := strings.TrimSpace(*input.Plan)
		if value == "" || len(value) > 64 {
			clientError(response, 400, "Invalid user update")
			return
		}
		input.Plan = &value
	}
	if input.Name != nil {
		value := strings.TrimSpace(*input.Name)
		if len(value) > 120 {
			clientError(response, 400, "Invalid user update")
			return
		}
		input.Name = &value
	}
	if input.Email != nil {
		value, valid := normalizedEmail(*input.Email)
		if !valid {
			clientError(response, 400, "Invalid user update")
			return
		}
		input.Email = &value
	}
	if input.LimitUnits != nil && (*input.LimitUnits < 0 || *input.LimitUnits > 10_000_000) {
		clientError(response, 400, "Invalid user update")
		return
	}
	if userID == claims.Subject && (input.Role != nil || input.Status != nil) {
		clientError(response, 400, "Cannot change your own role or status")
		return
	}
	ctx := request.Context()
	tx, err := server.repository.pool.Begin(ctx)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	defer tx.Rollback(ctx)
	var oldName, oldEmail, oldRole, oldStatus, oldPlan string
	if err = tx.QueryRow(ctx, `SELECT COALESCE(full_name,email),email,role,status,plan FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&oldName, &oldEmail, &oldRole, &oldStatus, &oldPlan); errors.Is(err, pgx.ErrNoRows) {
		clientError(response, 404, "User not found")
		return
	} else if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	var oldLimit int
	if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT limit_units FROM user_quotas WHERE user_id=$1 AND period_start<=now() AND period_end>now() LIMIT 1),0)`, userID).Scan(&oldLimit); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	if input.Email != nil && *input.Email != oldEmail {
		var emailInUse bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE lower(email)=lower($1) AND id <> $2)`, *input.Email, userID).Scan(&emailInUse); err != nil {
			clientError(response, 500, "Internal server error")
			return
		}
		if emailInUse {
			clientError(response, 409, "Email is already registered")
			return
		}
	}
	changes := map[string]any{}
	if input.Name != nil && *input.Name != oldName {
		changes["name"] = map[string]string{"from": oldName, "to": *input.Name}
	}
	if input.Email != nil && *input.Email != oldEmail {
		changes["email"] = map[string]string{"from": oldEmail, "to": *input.Email}
	}
	if input.Role != nil && *input.Role != oldRole {
		changes["role"] = map[string]string{"from": oldRole, "to": *input.Role}
	}
	if input.Status != nil && *input.Status != oldStatus {
		changes["status"] = map[string]string{"from": oldStatus, "to": *input.Status}
	}
	if input.Plan != nil && *input.Plan != oldPlan {
		changes["plan"] = map[string]string{"from": oldPlan, "to": *input.Plan}
	}
	if input.LimitUnits != nil && *input.LimitUnits != oldLimit {
		changes["limitUnits"] = map[string]int{"from": oldLimit, "to": *input.LimitUnits}
	}
	var version int
	var accessChangedEvent *RevocationEvent
	err = tx.QueryRow(ctx, `UPDATE users SET role=COALESCE($1,role),status=COALESCE($2,status),plan=COALESCE($3,plan),full_name=COALESCE($4,full_name),email=COALESCE($5,email),auth_version=auth_version+CASE WHEN $1::text IS NULL AND $2::text IS NULL THEN 0 ELSE 1 END WHERE id=$6 RETURNING auth_version`, input.Role, input.Status, input.Plan, input.Name, input.Email, userID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		clientError(response, 404, "User not found")
		return
	}
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	if input.LimitUnits != nil {
		_, err = tx.Exec(ctx, `INSERT INTO user_quotas(user_id,period_start,period_end,limit_units,used_units) VALUES($1,date_trunc('month',now()),date_trunc('month',now())+interval '1 month',$2,0) ON CONFLICT(user_id) DO UPDATE SET period_start=CASE WHEN NOT (user_quotas.period_start<=now() AND user_quotas.period_end>now()) THEN EXCLUDED.period_start ELSE user_quotas.period_start END,period_end=CASE WHEN NOT (user_quotas.period_start<=now() AND user_quotas.period_end>now()) THEN EXCLUDED.period_end ELSE user_quotas.period_end END,limit_units=EXCLUDED.limit_units,used_units=CASE WHEN NOT (user_quotas.period_start<=now() AND user_quotas.period_end>now()) THEN 0 ELSE user_quotas.used_units END,updated_at=now()`, userID, *input.LimitUnits)
		if err != nil {
			clientError(response, 500, "Internal server error")
			return
		}
	}
	if input.Role != nil || input.Status != nil {
		_, err = tx.Exec(ctx, `UPDATE sessions SET revoked_at=COALESCE(revoked_at,now()) WHERE user_id=$1`, userID)
		if err != nil {
			clientError(response, 500, "Internal server error")
			return
		}
		event := RevocationEvent{Type: "UserAccessChanged", UserID: userID, AuthVersion: version, Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}
		if err = server.repository.Publish(ctx, tx, event); err != nil {
			clientError(response, 500, "Internal server error")
			return
		}
		accessChangedEvent = &event
	}
	details, _ := json.Marshal(map[string]any{"changes": changes})
	if _, err = tx.Exec(ctx, `INSERT INTO admin_audit_logs(actor_id,target_user_id,action,details) VALUES($1,$2,'user.updated',$3::jsonb)`, claims.Subject, userID, details); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	if accessChangedEvent != nil {
		server.revocations.Apply(*accessChangedEvent)
	}
	response.WriteHeader(http.StatusNoContent)
}

// adminResetUserPassword lets an administrator issue a temporary password for
// another account. It invalidates all existing sessions so the old password or
// refresh token cannot be used after the reset.
func (server *Server) adminResetUserPassword(response http.ResponseWriter, request *http.Request, userID string) {
	claims, ok := server.requireAdmin(response, request)
	if !ok {
		return
	}
	if userID == claims.Subject {
		clientError(response, http.StatusBadRequest, "Use the change-password endpoint for your own account")
		return
	}
	var input adminPasswordResetInput
	if decodeJSON(request, &input) != nil || validatePassword(input.NewPassword) != "" {
		clientError(response, http.StatusBadRequest, "Invalid password reset")
		return
	}
	nextHash, err := server.passwords.Hash(input.NewPassword)
	if err != nil {
		clientError(response, http.StatusInternalServerError, "Internal server error")
		return
	}
	ctx := request.Context()
	tx, err := server.repository.pool.Begin(ctx)
	if err != nil {
		clientError(response, http.StatusInternalServerError, "Internal server error")
		return
	}
	defer tx.Rollback(ctx)
	var version int
	if err = tx.QueryRow(ctx, `UPDATE users SET password_hash=$1,must_change_password=true,auth_version=auth_version+1 WHERE id=$2 RETURNING auth_version`, nextHash, userID).Scan(&version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			clientError(response, http.StatusNotFound, "User not found")
		} else {
			clientError(response, http.StatusInternalServerError, "Internal server error")
		}
		return
	}
	if _, err = tx.Exec(ctx, `UPDATE sessions SET revoked_at=COALESCE(revoked_at,now()) WHERE user_id=$1 AND revoked_at IS NULL`, userID); err != nil {
		clientError(response, http.StatusInternalServerError, "Internal server error")
		return
	}
	details, _ := json.Marshal(map[string]any{"passwordReset": "Temporary password issued; existing sessions revoked"})
	if _, err = tx.Exec(ctx, `INSERT INTO admin_audit_logs(actor_id,target_user_id,action,details) VALUES($1,$2,'user.password_reset',$3::jsonb)`, claims.Subject, userID, details); err != nil {
		clientError(response, http.StatusInternalServerError, "Internal server error")
		return
	}
	event := RevocationEvent{Type: "PasswordChanged", UserID: userID, AuthVersion: version, Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = server.repository.Publish(ctx, tx, event); err != nil {
		clientError(response, http.StatusInternalServerError, "Internal server error")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		clientError(response, http.StatusInternalServerError, "Internal server error")
		return
	}
	server.revocations.Apply(event)
	slog.Info("admin_password_reset", "actor_id", claims.Subject, "user_id", userID)
	response.WriteHeader(http.StatusNoContent)
}

func (server *Server) adminDeleteUser(response http.ResponseWriter, request *http.Request, userID string) {
	claims, ok := server.requireAdmin(response, request)
	if !ok {
		return
	}
	if userID == claims.Subject {
		clientError(response, 400, "Cannot delete your own account")
		return
	}
	ctx := request.Context()
	tx, err := server.repository.pool.Begin(ctx)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	defer tx.Rollback(ctx)
	var name, email string
	if err = tx.QueryRow(ctx, `DELETE FROM users WHERE id=$1 RETURNING COALESCE(full_name,email),email`, userID).Scan(&name, &email); errors.Is(err, pgx.ErrNoRows) {
		clientError(response, 404, "User not found")
		return
	} else if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	details, _ := json.Marshal(map[string]any{"deleted": map[string]string{"name": name, "email": email, "userId": userID}})
	if _, err = tx.Exec(ctx, `INSERT INTO admin_audit_logs(actor_id,action,details) VALUES($1,'user.deleted',$2::jsonb)`, claims.Subject, details); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (server *Server) adminAudit(response http.ResponseWriter, request *http.Request) {
	if _, ok := server.requireAdmin(response, request); !ok {
		return
	}
	rows, err := server.repository.pool.Query(request.Context(), `SELECT a.id,COALESCE(actor.email,''),COALESCE(target.email,''),a.action,a.details,a.created_at FROM admin_audit_logs a LEFT JOIN users actor ON actor.id=a.actor_id LEFT JOIN users target ON target.id=a.target_user_id ORDER BY a.created_at DESC LIMIT 100`)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	defer rows.Close()
	logs := make([]map[string]any, 0)
	for rows.Next() {
		var id int64
		var actor, target, action string
		var details json.RawMessage
		var created time.Time
		if err := rows.Scan(&id, &actor, &target, &action, &details, &created); err != nil {
			clientError(response, 500, "Internal server error")
			return
		}
		logs = append(logs, map[string]any{"id": id, "actor": actor, "target": target, "action": action, "details": json.RawMessage(details), "createdAt": created})
	}
	if rows.Err() != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	writeJSON(response, 200, map[string]any{"logs": logs})
}

func (server *Server) route(response http.ResponseWriter, request *http.Request) {
	claims, ok := server.authenticate(response, request, "route:calculate")
	if !ok {
		return
	}
	ctx := request.Context()
	if err := server.consumeQuota(ctx, claims.Subject, "route"); err != nil {
		if err.Error() != "quota exceeded" {
			clientError(response, 500, "Internal server error")
			return
		}
		clientError(response, 429, "Quota exceeded")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 128*1024))
	if err != nil {
		clientError(response, 400, "Invalid route request")
		return
	}
	workerRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, server.config.ValhallaURL+"/route", strings.NewReader(string(body)))
	if err != nil {
		clientError(response, 502, "Server unavailable")
		return
	}
	workerRequest.Header.Set("Content-Type", "application/json")
	workerRequest.Header.Set("X-Internal-User-Id", claims.Subject)
	workerRequest.Header.Set("X-Internal-Session-Id", claims.SessionID)
	workerRequest.Header.Set("X-Internal-Scopes", strings.Join(claims.Scopes, " "))
	workerRequest.Header.Set("X-Internal-Plan", claims.Plan)
	workerRequest.Header.Set("X-Request-Id", request.Header.Get("X-Request-Id"))
	workerResponse, err := server.workerClient.Do(workerRequest)
	if err != nil {
		clientError(response, 502, "Server unavailable")
		return
	}
	defer workerResponse.Body.Close()
	response.Header().Set("Content-Type", workerResponse.Header.Get("Content-Type"))
	response.WriteHeader(workerResponse.StatusCode)
	_, _ = io.Copy(response, io.LimitReader(workerResponse.Body, 10*1024*1024))
}

// elevation keeps the Valhalla height service private while returning a bounded,
// uniformly sampled profile for an authenticated route request.
func (server *Server) elevation(response http.ResponseWriter, request *http.Request) {
	claims, ok := server.authenticate(response, request, "route:calculate")
	if !ok {
		return
	}
	var input elevationInput
	if err := decodeJSON(request, &input); err != nil || len(input.Shape) < 2 || len(input.Shape) > 1000 || input.ResampleDistance < 25 || input.ResampleDistance > 1000 {
		clientError(response, 400, "Invalid elevation request")
		return
	}
	for _, location := range input.Shape {
		if math.IsNaN(location.Latitude) || math.IsInf(location.Latitude, 0) || math.IsNaN(location.Longitude) || math.IsInf(location.Longitude, 0) || location.Latitude < -90 || location.Latitude > 90 || location.Longitude < -180 || location.Longitude > 180 {
			clientError(response, 400, "Invalid elevation request")
			return
		}
	}
	body, err := json.Marshal(map[string]any{
		"shape":             input.Shape,
		"range":             true,
		"resample_distance": input.ResampleDistance,
		"height_precision":  1,
	})
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	workerRequest, err := http.NewRequestWithContext(request.Context(), http.MethodPost, server.config.ValhallaURL+"/height", strings.NewReader(string(body)))
	if err != nil {
		clientError(response, 502, "Server unavailable")
		return
	}
	workerRequest.Header.Set("Content-Type", "application/json")
	workerRequest.Header.Set("X-Internal-User-Id", claims.Subject)
	workerRequest.Header.Set("X-Internal-Session-Id", claims.SessionID)
	workerRequest.Header.Set("X-Internal-Scopes", strings.Join(claims.Scopes, " "))
	workerRequest.Header.Set("X-Internal-Plan", claims.Plan)
	workerRequest.Header.Set("X-Request-Id", request.Header.Get("X-Request-Id"))
	workerResponse, err := server.workerClient.Do(workerRequest)
	if err != nil {
		clientError(response, 502, "Server unavailable")
		return
	}
	defer workerResponse.Body.Close()
	response.Header().Set("Content-Type", workerResponse.Header.Get("Content-Type"))
	response.WriteHeader(workerResponse.StatusCode)
	_, _ = io.Copy(response, io.LimitReader(workerResponse.Body, 2*1024*1024))
}

// tileProxy keeps the worker private: browser requests always enter through the
// Controller, while the Controller is the only workload allowed to reach Tile Server.
// Basemap files remain cacheable map assets and do not consume user quota.
func (server *Server) tileProxy(response http.ResponseWriter, request *http.Request, workerPath string) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		clientError(response, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if workerPath == "" {
		workerPath = "/"
	}
	if !strings.HasPrefix(workerPath, "/") {
		clientError(response, http.StatusNotFound, "Not found")
		return
	}
	for _, segment := range strings.Split(workerPath, "/") {
		if segment == "." || segment == ".." || strings.Contains(segment, "\\") {
			clientError(response, http.StatusNotFound, "Not found")
			return
		}
	}
	target := server.config.TileServerURL + workerPath
	if request.URL.RawQuery != "" {
		target += "?" + request.URL.RawQuery
	}
	workerRequest, err := http.NewRequestWithContext(request.Context(), request.Method, target, nil)
	if err != nil {
		clientError(response, http.StatusBadGateway, "Tile service unavailable")
		return
	}
	for _, header := range []string{"Accept", "Accept-Encoding", "If-Modified-Since", "If-None-Match", "Range"} {
		if value := request.Header.Get(header); value != "" {
			workerRequest.Header.Set(header, value)
		}
	}
	workerRequest.Header.Set("X-Request-Id", request.Header.Get("X-Request-Id"))
	workerResponse, err := server.workerClient.Do(workerRequest)
	if err != nil {
		clientError(response, http.StatusBadGateway, "Tile service unavailable")
		return
	}
	defer workerResponse.Body.Close()
	for _, header := range []string{"Accept-Ranges", "Cache-Control", "Content-Encoding", "Content-Length", "Content-Type", "ETag", "Last-Modified", "Vary"} {
		if value := workerResponse.Header.Get(header); value != "" {
			response.Header().Set(header, value)
		}
	}
	response.WriteHeader(workerResponse.StatusCode)
	if request.Method != http.MethodHead {
		_, _ = io.Copy(response, workerResponse.Body)
	}
}

// geoDataProxy keeps GeoData private while preserving its release visibility
// boundary. Authenticated callers must have map:read and their JWT is forwarded
// so GeoData can resolve user/organization grants. Anonymous callers are
// forwarded without a JWT: GeoData exposes only public releases to them and
// rejects restricted releases itself. This lets an unauthenticated share link
// render a public basemap without opening restricted data.
func (server *Server) geoDataProxy(response http.ResponseWriter, request *http.Request, workerPath string) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		clientError(response, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if workerPath == "" {
		workerPath = "/"
	}
	if !strings.HasPrefix(workerPath, "/") {
		clientError(response, http.StatusNotFound, "Not found")
		return
	}
	for _, segment := range strings.Split(workerPath, "/") {
		if segment == "." || segment == ".." || strings.Contains(segment, "\\") {
			clientError(response, http.StatusNotFound, "Not found")
			return
		}
	}
	if strings.TrimSpace(request.Header.Get("Authorization")) != "" {
		if _, ok := server.authenticate(response, request, "map:read"); !ok {
			return
		}
	}
	target := server.config.GeoDataURL + workerPath
	if request.URL.RawQuery != "" {
		target += "?" + request.URL.RawQuery
	}
	workerRequest, err := http.NewRequestWithContext(request.Context(), request.Method, target, nil)
	if err != nil {
		clientError(response, http.StatusBadGateway, "GeoData service unavailable")
		return
	}
	for _, header := range []string{"Accept", "Accept-Encoding", "Authorization", "If-Modified-Since", "If-None-Match", "Range"} {
		if value := request.Header.Get(header); value != "" {
			workerRequest.Header.Set(header, value)
		}
	}
	workerRequest.Header.Set("X-Request-Id", request.Header.Get("X-Request-Id"))
	workerResponse, err := server.workerClient.Do(workerRequest)
	if err != nil {
		clientError(response, http.StatusBadGateway, "GeoData service unavailable")
		return
	}
	defer workerResponse.Body.Close()
	for _, header := range []string{"Accept-Ranges", "Cache-Control", "Content-Encoding", "Content-Length", "Content-Type", "ETag", "Last-Modified", "Vary"} {
		if value := workerResponse.Header.Get(header); value != "" {
			response.Header().Set(header, value)
		}
	}
	response.WriteHeader(workerResponse.StatusCode)
	if request.Method != http.MethodHead {
		_, _ = io.Copy(response, workerResponse.Body)
	}
}

// nominatimProxy provides the public geocoding API through the Controller so
// the Nominatim worker remains a private ClusterIP service.
func (server *Server) nominatimProxy(response http.ResponseWriter, request *http.Request, workerPath string) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		clientError(response, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if workerPath == "" {
		workerPath = "/"
	}
	if !strings.HasPrefix(workerPath, "/") {
		clientError(response, http.StatusNotFound, "Not found")
		return
	}
	for _, segment := range strings.Split(workerPath, "/") {
		if segment == "." || segment == ".." || strings.Contains(segment, "\\") {
			clientError(response, http.StatusNotFound, "Not found")
			return
		}
	}
	claims, ok := server.authenticate(response, request, "map:read")
	if !ok {
		return
	}
	if request.Method == http.MethodGet {
		if err := server.consumeQuota(request.Context(), claims.Subject, "search"); err != nil {
			if err.Error() == "quota exceeded" {
				clientError(response, http.StatusTooManyRequests, "Quota exceeded")
			} else {
				clientError(response, http.StatusInternalServerError, "Internal server error")
			}
			return
		}
	}

	target := server.config.NominatimURL + workerPath
	if request.URL.RawQuery != "" {
		target += "?" + request.URL.RawQuery
	}
	workerRequest, err := http.NewRequestWithContext(request.Context(), request.Method, target, nil)
	if err != nil {
		clientError(response, http.StatusBadGateway, "Server unavailable")
		return
	}
	for _, header := range []string{"Accept", "Accept-Encoding", "If-Modified-Since", "If-None-Match"} {
		if value := request.Header.Get(header); value != "" {
			workerRequest.Header.Set(header, value)
		}
	}
	workerRequest.Header.Set("X-Request-Id", request.Header.Get("X-Request-Id"))
	workerResponse, err := server.workerClient.Do(workerRequest)
	if err != nil {
		clientError(response, http.StatusBadGateway, "Server unavailable")
		return
	}
	defer workerResponse.Body.Close()
	for _, header := range []string{"Cache-Control", "Content-Encoding", "Content-Length", "Content-Type", "ETag", "Last-Modified", "Vary"} {
		if value := workerResponse.Header.Get(header); value != "" {
			response.Header().Set(header, value)
		}
	}
	response.WriteHeader(workerResponse.StatusCode)
	if request.Method != http.MethodHead {
		_, _ = io.Copy(response, workerResponse.Body)
	}
}

func (server *Server) disableUser(response http.ResponseWriter, request *http.Request, userID string) {
	_, ok := server.requireAdmin(response, request)
	if !ok {
		return
	}
	ctx := request.Context()
	tx, err := server.repository.pool.Begin(ctx)
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	defer tx.Rollback(ctx)
	var version int
	err = tx.QueryRow(ctx, `UPDATE users SET status='DISABLED',auth_version=auth_version+1 WHERE id=$1 AND status <> 'DISABLED' RETURNING auth_version`, userID).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		clientError(response, 404, "User not found")
		return
	}
	if err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	event := RevocationEvent{Type: "UserDisabled", UserID: userID, AuthVersion: version, Timestamp: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = server.repository.Publish(ctx, tx, event); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	if err = tx.Commit(ctx); err != nil {
		clientError(response, 500, "Internal server error")
		return
	}
	server.revocations.Apply(event)
	response.WriteHeader(http.StatusNoContent)
}

func (server *Server) cors(response http.ResponseWriter, request *http.Request) bool {
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	allowed := false
	for _, value := range strings.Split(server.config.AppOrigins, ",") {
		if strings.TrimSpace(value) == origin {
			allowed = true
			break
		}
	}
	if !allowed {
		clientError(response, http.StatusForbidden, "Origin not allowed")
		return false
	}
	response.Header().Set("Access-Control-Allow-Origin", origin)
	response.Header().Set("Access-Control-Allow-Credentials", "true")
	response.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-CSRF-Token, X-Request-Id")
	response.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
	return true
}

func (server *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	requestID := newRequestID()
	request.Header.Set("X-Request-Id", requestID)
	response.Header().Set("X-Request-Id", requestID)
	response.Header().Set("X-Content-Type-Options", "nosniff")
	if !server.cors(response, request) {
		return
	}
	if request.Method == http.MethodOptions {
		response.WriteHeader(http.StatusNoContent)
		return
	}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/api/openapi.json":
		writeJSON(response, http.StatusOK, openAPISpec())
	case request.Method == http.MethodGet && request.URL.Path == "/health":
		if server.revocations.Ready() {
			writeJSON(response, 200, map[string]bool{"ok": true})
		} else {
			writeJSON(response, 503, map[string]bool{"ok": false})
		}
	case request.Method == http.MethodGet && request.URL.Path == "/auth/.well-known/jwks.json":
		writeJSON(response, 200, server.tokens.PublicJWKS())
	case request.Method == http.MethodPost && request.URL.Path == "/auth/register":
		server.register(response, request)
	case request.Method == http.MethodPost && request.URL.Path == "/auth/login":
		server.login(response, request)
	case request.Method == http.MethodGet && request.URL.Path == "/auth/session-events":
		server.sessionEvents(response, request)
	case request.Method == http.MethodPost && request.URL.Path == "/auth/refresh":
		server.refresh(response, request)
	case request.Method == http.MethodPost && request.URL.Path == "/auth/logout":
		server.logout(response, request, false)
	case request.Method == http.MethodPost && request.URL.Path == "/auth/logout-all":
		server.logout(response, request, true)
	case request.Method == http.MethodPost && request.URL.Path == "/auth/change-password":
		server.changePassword(response, request)
	case request.Method == http.MethodGet && request.URL.Path == "/auth/me":
		server.profile(response, request, false)
	case request.Method == http.MethodPatch && request.URL.Path == "/auth/me":
		server.profile(response, request, true)
	case request.Method == http.MethodGet && request.URL.Path == "/api/billing":
		server.billing(response, request)
	case request.Method == http.MethodGet && request.URL.Path == "/api/admin/dashboard":
		server.adminDashboard(response, request)
	case request.Method == http.MethodGet && request.URL.Path == "/api/admin/billing":
		server.adminBilling(response, request)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/admin/billing/"):
		userID := strings.TrimPrefix(request.URL.Path, "/api/admin/billing/")
		if userID == "" || strings.Contains(userID, "/") {
			clientError(response, 400, "Invalid user id")
		} else {
			server.adminBillingUser(response, request, userID)
		}
	case request.Method == http.MethodGet && request.URL.Path == "/api/admin/users":
		server.adminUsers(response, request)
	case request.Method == http.MethodPost && request.URL.Path == "/api/admin/users":
		server.adminCreateUser(response, request)
	case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/api/admin/users/") && strings.HasSuffix(request.URL.Path, "/reset-password"):
		userID := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/admin/users/"), "/reset-password")
		if userID == "" || strings.Contains(userID, "/") {
			clientError(response, 400, "Invalid user id")
		} else {
			server.adminResetUserPassword(response, request, userID)
		}
	case request.Method == http.MethodGet && request.URL.Path == "/api/admin/audit":
		server.adminAudit(response, request)
	case request.Method == http.MethodPatch && strings.HasPrefix(request.URL.Path, "/api/admin/users/"):
		userID := strings.TrimPrefix(request.URL.Path, "/api/admin/users/")
		if userID == "" || strings.Contains(userID, "/") {
			clientError(response, 400, "Invalid user id")
		} else {
			server.adminUpdateUser(response, request, userID)
		}
	case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/api/admin/users/"):
		userID := strings.TrimPrefix(request.URL.Path, "/api/admin/users/")
		if userID == "" || strings.Contains(userID, "/") {
			clientError(response, 400, "Invalid user id")
		} else {
			server.adminDeleteUser(response, request, userID)
		}
	case request.Method == http.MethodPost && request.URL.Path == "/api/gateway/route":
		server.route(response, request)
	case request.Method == http.MethodPost && request.URL.Path == "/api/gateway/elevation":
		server.elevation(response, request)
	case request.Method == http.MethodPost && request.URL.Path == "/api/shares":
		server.createGeoJSONShare(response, request)
	case request.Method == http.MethodGet && request.URL.Path == "/api/shares":
		server.listGeoJSONShares(response, request)
	case request.Method == http.MethodGet && request.URL.Path == "/api/shares/recipients":
		server.searchGeoJSONShareRecipients(response, request)
	case request.Method == http.MethodGet && request.URL.Path == "/api/shares/received":
		server.listReceivedGeoJSONShares(response, request)
	case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/api/shares/") && strings.HasSuffix(request.URL.Path, "/link"):
		id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/shares/"), "/link")
		if strings.Contains(id, "/") || id == "" {
			clientError(response, http.StatusBadRequest, "Invalid share id")
		} else {
			server.getOrCreateGeoJSONShareLink(response, request, id)
		}
	case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/api/shares/") && strings.HasSuffix(request.URL.Path, "/recipients"):
		id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/shares/"), "/recipients")
		if strings.Contains(id, "/") || id == "" {
			clientError(response, http.StatusBadRequest, "Invalid share id")
		} else {
			server.sendGeoJSONShare(response, request, id)
		}
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/shares/received/"):
		server.getReceivedGeoJSONShare(response, request, strings.TrimPrefix(request.URL.Path, "/api/shares/received/"))
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/shares/"):
		identifier := strings.TrimPrefix(request.URL.Path, "/api/shares/")
		if strings.Contains(identifier, "/") || identifier == "" {
			clientError(response, http.StatusNotFound, "Share not found")
		} else {
			server.getGeoJSONShare(response, request, identifier)
		}
	case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/api/shares/"):
		server.revokeGeoJSONShare(response, request, strings.TrimPrefix(request.URL.Path, "/api/shares/"))
	case request.Method == http.MethodGet && request.URL.Path == "/api/gateway/map":
		if claims, ok := server.authenticate(response, request, "map:read"); ok {
			writeJSON(response, 200, map[string]any{"user_id": claims.Subject, "plan": claims.Plan, "request_id": request.Header.Get("X-Request-Id")})
		}
	case (request.Method == http.MethodGet || request.Method == http.MethodHead) && request.URL.Path == "/api/tile-catalog":
		server.tileProxy(response, request, "/")
	case (request.Method == http.MethodGet || request.Method == http.MethodHead) && request.URL.Path == "/api/geodata/catalog":
		server.geoDataProxy(response, request, "/v1/catalog")
	case (request.Method == http.MethodGet || request.Method == http.MethodHead) && strings.HasPrefix(request.URL.Path, "/api/geodata/"):
		server.geoDataProxy(response, request, strings.TrimPrefix(request.URL.Path, "/api/geodata"))
	case (request.Method == http.MethodGet || request.Method == http.MethodHead) && request.URL.Path == "/api/tiles":
		server.tileProxy(response, request, "/")
	case (request.Method == http.MethodGet || request.Method == http.MethodHead) && strings.HasPrefix(request.URL.Path, "/api/tiles/"):
		server.tileProxy(response, request, strings.TrimPrefix(request.URL.Path, "/api/tiles"))
	case (request.Method == http.MethodGet || request.Method == http.MethodHead) && request.URL.Path == "/api/nominatim":
		server.nominatimProxy(response, request, "/")
	case (request.Method == http.MethodGet || request.Method == http.MethodHead) && strings.HasPrefix(request.URL.Path, "/api/nominatim/"):
		server.nominatimProxy(response, request, strings.TrimPrefix(request.URL.Path, "/api/nominatim"))
	case request.Method == http.MethodGet && request.URL.Path == "/api/suggestions":
		server.suggestions(response, request)
	case request.Method == http.MethodPost && request.URL.Path == "/api/spatial-search":
		server.spatialSearch(response, request)
	case request.Method == http.MethodPost && strings.HasPrefix(request.URL.Path, "/api/admin/users/") && strings.HasSuffix(request.URL.Path, "/disable"):
		userID := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/admin/users/"), "/disable")
		if userID == "" || strings.Contains(userID, "/") {
			clientError(response, 400, "Invalid user id")
		} else {
			server.disableUser(response, request, userID)
		}
	default:
		clientError(response, 404, "Not found")
	}
}

func (server *Server) Shutdown(context.Context) error { return nil }
