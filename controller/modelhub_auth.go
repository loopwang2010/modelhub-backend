// Modelhub auth + wallet bridge — S11 + S12 (per BLUEPRINT.md and
// S11-S13-OPS-DESIGN.md).
//
// This file wires the inherited new-api session machinery to the
// internal/wallet package (S6) so that:
//
//   - Registering a user atomically provisions a wallet_account row
//     (per ADR-013, AccountID = "user:{user_id}").
//   - /v1/auth/me returns the logged-in user's identity AND current
//     wallet balance in one round-trip — the frontend's "am I logged in"
//     probe.
//   - /v1/wallet/balance and /v1/wallet/history are self-only views.
//   - /admin/wallet/topup writes a regular ledger row through wallet.Topup
//     (per AP-8 — NO direct UPDATE on balance) plus an audit row attributing
//     the credit to an admin.
//   - /admin/wallet/history paginates the ledger for one user.
//
// The wallet package is consumed via a package-level singleton wired in
// main.go. Tests inject their own *wallet.DBWallet via SetWallet.

package controller

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/adapter"
	"github.com/QuantumNous/new-api/internal/wallet"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

// ─────────────────────────────────────────────────────────────────────────
// Wallet singleton wiring
// ─────────────────────────────────────────────────────────────────────────

var (
	walletMu           sync.RWMutex
	walletInstance     wallet.Wallet
	walletRawDB        *sql.DB // for raw history queries that wallet.Wallet doesn't expose
	walletDialect      wallet.Dialect
	walletDisabled     bool                     // true when wallet is unwired
	walletPendingStore wallet.PendingCostStore  // F5: shared store for the wallet subscriber
)

// SetWallet installs a wallet instance. Used by main.go in production and
// by tests to inject a SQLite-backed instance.
func SetWallet(w wallet.Wallet, db *sql.DB, dialect wallet.Dialect) {
	walletMu.Lock()
	defer walletMu.Unlock()
	walletInstance = w
	walletRawDB = db
	walletDialect = dialect
	walletDisabled = (w == nil)
}

// SetWalletPendingStore installs the PendingCostStore that the wallet
// subscriber will use to bridge TaskSucceeded → AssetHosted. Defaults
// to in-memory when WALLET_PENDING_STORE is unset; switches to Redis
// when WALLET_PENDING_STORE=redis (with REDIS_URL configured).
//
// Stored on the controller package so the eventual subscriber wiring
// (when EventBus startup lands in main.go) can read the same instance.
func SetWalletPendingStore(store wallet.PendingCostStore) {
	walletMu.Lock()
	defer walletMu.Unlock()
	walletPendingStore = store
}

// GetWalletPendingStore returns the configured pending-cost store, or
// nil if SetWalletPendingStore was never called. Exposed so the
// subscriber wiring (and tests) can resolve the singleton without
// re-reading env.
func GetWalletPendingStore() wallet.PendingCostStore {
	walletMu.RLock()
	defer walletMu.RUnlock()
	return walletPendingStore
}

// getWallet returns the configured wallet, or nil if not wired.
func getWallet() wallet.Wallet {
	walletMu.RLock()
	defer walletMu.RUnlock()
	return walletInstance
}

func getWalletDB() (*sql.DB, wallet.Dialect) {
	walletMu.RLock()
	defer walletMu.RUnlock()
	return walletRawDB, walletDialect
}

func walletIsDisabled() bool {
	walletMu.RLock()
	defer walletMu.RUnlock()
	return walletDisabled
}

// ensureWalletAccountForUser is the post-registration hook. On the inherited
// new-api Register path, after the user row commits, we provision a
// wallet_account row using the canonical AccountID "user:{userID}".
//
// EnsureAccount is itself idempotent (ON CONFLICT DO NOTHING), so retries
// are safe. We log on failure but do not roll back the user — the wallet is
// degraded, not broken; an admin can re-run EnsureAccount via top-up. (The
// alternative — an atomic two-system tx — is impractical because GORM owns
// the user row and the wallet owns its own *sql.DB connection.)
func ensureWalletAccountForUser(ctx context.Context, userID int) {
	w := getWallet()
	if w == nil {
		// Wallet not wired — silently skip (tests, or non-modelhub deployments).
		return
	}
	accountID := wallet.UserAccountID(strconv.Itoa(userID))
	if err := w.EnsureAccount(ctx, accountID, wallet.AccountKindUserWallet, strconv.Itoa(userID)); err != nil {
		common.SysLog(fmt.Sprintf(
			"modelhub: wallet EnsureAccount failed for user %d: %v (wallet degraded — admin retry required)",
			userID, err,
		))
	}
}

// ─────────────────────────────────────────────────────────────────────────
// /v1/auth/me — identity + balance
// ─────────────────────────────────────────────────────────────────────────

// AuthMe returns the current session's identity along with the wallet
// balance (in micro-USD). Callers MUST be wrapped in middleware.UserAuth.
func AuthMe(c *gin.Context) {
	id := c.GetInt("id")
	if id == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error":   gin.H{"code": "unauthenticated", "message": "no session"},
		})
		return
	}
	user, err := model.GetUserById(id, false)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	accountID := wallet.UserAccountID(strconv.Itoa(user.Id))
	balance := readBalanceMicroUSD(c.Request.Context(), accountID)

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"user_id":           user.Id,
			"account_id":        string(accountID),
			"email":             user.Email,
			"username":          user.Username,
			"role":              user.Role,
			"balance_micro_usd": int64(balance),
		},
	})
}

// readBalanceMicroUSD returns 0 when the wallet is not wired (instead of
// erroring) so the /v1/auth/me probe still succeeds in degraded mode.
func readBalanceMicroUSD(ctx context.Context, accountID wallet.AccountID) adapter.CostUSD {
	w := getWallet()
	if w == nil {
		return 0
	}
	bal, err := w.Balance(ctx, accountID)
	if err != nil {
		// The wallet returns an error when the account does not exist (no
		// ledger rows). That's normal for a freshly-registered user before
		// any top-up; treat as zero balance.
		common.SysLog(fmt.Sprintf("modelhub: Balance(%s) error (treating as 0): %v", accountID, err))
		return 0
	}
	return bal
}

// ─────────────────────────────────────────────────────────────────────────
// /v1/wallet/balance — self
// ─────────────────────────────────────────────────────────────────────────

func WalletBalanceSelf(c *gin.Context) {
	id := c.GetInt("id")
	if id == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error":   gin.H{"code": "unauthenticated", "message": "no session"},
		})
		return
	}
	accountID := wallet.UserAccountID(strconv.Itoa(id))
	bal := readBalanceMicroUSD(c.Request.Context(), accountID)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"account_id":        string(accountID),
			"balance_micro_usd": int64(bal),
		},
	})
}

// ─────────────────────────────────────────────────────────────────────────
// /v1/wallet/history — self
// ─────────────────────────────────────────────────────────────────────────

const (
	defaultHistoryLimit = 50
	maxHistoryLimit     = 500
)

// WalletHistorySelf returns the most recent ledger entries for the
// caller's wallet. Pagination uses simple LIMIT only — IDs are in
// chronological order so a client can use the smallest ID returned as a
// "before" cursor in a follow-up query (Sprint 2).
func WalletHistorySelf(c *gin.Context) {
	id := c.GetInt("id")
	if id == 0 {
		c.JSON(http.StatusUnauthorized, gin.H{
			"success": false,
			"error":   gin.H{"code": "unauthenticated", "message": "no session"},
		})
		return
	}
	limit := parseLimit(c.Query("limit"))
	accountID := wallet.UserAccountID(strconv.Itoa(id))
	rows, err := readLedger(c.Request.Context(), string(accountID), limit)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"account_id": string(accountID),
			"entries":    rows,
			"limit":      limit,
		},
	})
}

// ─────────────────────────────────────────────────────────────────────────
// /admin/wallet/topup — admin
// ─────────────────────────────────────────────────────────────────────────

// adminTopupRequest is the body of POST /admin/wallet/topup.
//
//	user_id    : int — target user (their wallet is "user:{user_id}")
//	amount_usd : float64 — credit amount in USD; converted to micro-USD
//	note       : string — free-form admin note (audit trail)
//	idem       : string — optional idempotency key (UUID-shaped recommended)
type adminTopupRequest struct {
	UserID    int     `json:"user_id"`
	AmountUSD float64 `json:"amount_usd"`
	Note      string  `json:"note"`
	Idem      string  `json:"idem"`
}

// AdminWalletTopup is the admin-only credit-grant endpoint. Per AP-8 and
// ADR-005, this writes through wallet.Topup (which writes a paired
// ledger op + audit row in a single tx). NO direct balance UPDATEs.
func AdminWalletTopup(c *gin.Context) {
	if walletIsDisabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"success": false,
			"error":   gin.H{"code": "wallet_unavailable", "message": "wallet subsystem not configured"},
		})
		return
	}
	adminID := c.GetInt("id")
	role := c.GetInt("role")
	if adminID == 0 || role < common.RoleAdminUser {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"error":   gin.H{"code": "forbidden", "message": "admin role required"},
		})
		return
	}

	var req adminTopupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   gin.H{"code": "invalid_request", "message": err.Error()},
		})
		return
	}
	if req.UserID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   gin.H{"code": "invalid_request", "message": "user_id required"},
		})
		return
	}
	if req.AmountUSD <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   gin.H{"code": "invalid_request", "message": "amount_usd must be positive"},
		})
		return
	}

	amountMicro := adapter.CostUSD(req.AmountUSD * 1_000_000)
	if amountMicro <= 0 || amountMicro > wallet.MaxTopupUSD {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   gin.H{"code": "invalid_amount", "message": "amount_usd out of permitted range"},
		})
		return
	}

	accountID := wallet.UserAccountID(strconv.Itoa(req.UserID))
	w := getWallet()

	// Make sure the target wallet account exists (defensive — Register's hook
	// should have created it, but a pre-S11 user wouldn't have one yet).
	if err := w.EnsureAccount(c.Request.Context(), accountID, wallet.AccountKindUserWallet, strconv.Itoa(req.UserID)); err != nil {
		common.ApiError(c, err)
		return
	}

	idemKey := adapter.IdempotencyKey(req.Idem)
	err := w.Topup(c.Request.Context(), accountID, amountMicro, req.Note, strconv.Itoa(adminID), idemKey)
	if err != nil {
		if errors.Is(err, wallet.ErrInvalidTopupAmount) {
			c.JSON(http.StatusBadRequest, gin.H{
				"success": false,
				"error":   gin.H{"code": "invalid_amount", "message": err.Error()},
			})
			return
		}
		common.ApiError(c, err)
		return
	}

	bal := readBalanceMicroUSD(c.Request.Context(), accountID)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"account_id":            string(accountID),
			"credited_micro_usd":    int64(amountMicro),
			"new_balance_micro_usd": int64(bal),
		},
	})
}

// ─────────────────────────────────────────────────────────────────────────
// /admin/wallet/history — admin paginated ledger view
// ─────────────────────────────────────────────────────────────────────────

func AdminWalletHistory(c *gin.Context) {
	role := c.GetInt("role")
	if role < common.RoleAdminUser {
		c.JSON(http.StatusForbidden, gin.H{
			"success": false,
			"error":   gin.H{"code": "forbidden", "message": "admin role required"},
		})
		return
	}
	uidStr := c.Query("user_id")
	if uidStr == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   gin.H{"code": "invalid_request", "message": "user_id query param required"},
		})
		return
	}
	uid, err := strconv.Atoi(uidStr)
	if err != nil || uid <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"error":   gin.H{"code": "invalid_request", "message": "user_id must be positive integer"},
		})
		return
	}
	limit := parseLimit(c.Query("limit"))
	accountID := wallet.UserAccountID(strconv.Itoa(uid))
	rows, err := readLedger(c.Request.Context(), string(accountID), limit)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	bal := readBalanceMicroUSD(c.Request.Context(), accountID)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"account_id":        string(accountID),
			"balance_micro_usd": int64(bal),
			"entries":           rows,
			"limit":             limit,
		},
	})
}

// ─────────────────────────────────────────────────────────────────────────
// Raw ledger reader (read-only — wallet package is the writer)
// ─────────────────────────────────────────────────────────────────────────

// LedgerEntry is the wire-format row returned by /v1/wallet/history and
// /admin/wallet/history. Stays a flat shape for easy frontend rendering.
type LedgerEntry struct {
	OperationID    string    `json:"operation_id"`
	AccountID      string    `json:"account_id"`
	AmountMicroUSD int64     `json:"amount_micro_usd"`
	ReasonCode     string    `json:"reason_code"`
	RefTaskID      string    `json:"ref_task_id,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// readLedger queries wallet_ledger directly. The wallet package owns the
// writer; the read-side projection is intentionally kept here to avoid
// growing the wallet.Wallet interface for query convenience.
func readLedger(ctx context.Context, accountID string, limit int) ([]LedgerEntry, error) {
	db, _ := getWalletDB()
	if db == nil {
		return nil, errors.New("wallet database not configured")
	}
	const q = `
		SELECT operation_id, account_id, amount_micro_usd, reason_code,
		       COALESCE(ref_task_id, ''), created_at
		  FROM wallet_ledger
		 WHERE account_id = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT ` // limit appended below; not user-controlled past parseLimit
	rows, err := db.QueryContext(ctx, q+strconv.Itoa(limit), accountID)
	if err != nil {
		return nil, fmt.Errorf("ledger query: %w", err)
	}
	defer rows.Close()

	out := make([]LedgerEntry, 0, limit)
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(&e.OperationID, &e.AccountID, &e.AmountMicroUSD,
			&e.ReasonCode, &e.RefTaskID, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("ledger scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// parseLimit clamps the requested page size to a safe range.
func parseLimit(raw string) int {
	if raw == "" {
		return defaultHistoryLimit
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return defaultHistoryLimit
	}
	if n > maxHistoryLimit {
		return maxHistoryLimit
	}
	return n
}

// ─────────────────────────────────────────────────────────────────────────
// /v1/auth/{register,login,logout} — modelhub-shaped auth wrappers
// ─────────────────────────────────────────────────────────────────────────
//
// The inherited new-api auth lives at /api/user/{register,login,logout} with
// {username, password} bodies and the {success, message} envelope. The
// modelhub-web SPA (packages/studio/src/modelhub-client.js) was written
// against an /v1/auth/* surface with {email, password} bodies and the
// modelhub {success, data|error} envelope. S11 mounted /v1/auth/me but the
// register/login/logout wrappers were never landed — this file closes the
// gap. The handlers reuse model.User.Insert / ValidateAndFill so password
// hashing and uniqueness checks stay on the inherited code path; the only
// translation is body shape, response envelope, and post-register auto-login.

const minPasswordLen = 8

// modelhubAuthRequest matches modelhub-client's register/login POST body.
type modelhubAuthRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// ModelhubRegister handles POST /v1/auth/register. Username is set to the
// email (S11 design intent — AuthMe returns both as the same logical value
// for modelhub users). On success the session is set immediately so the
// frontend's follow-up getMe() succeeds without a separate login round-trip.
func ModelhubRegister(c *gin.Context) {
	if !common.RegisterEnabled || !common.PasswordRegisterEnabled {
		modelhubErr(c, http.StatusForbidden, "registration_disabled", "registration is currently disabled")
		return
	}

	req, ok := decodeAuthBody(c)
	if !ok {
		return
	}
	if len(req.Password) < minPasswordLen {
		modelhubErr(c, http.StatusBadRequest, "invalid_password",
			fmt.Sprintf("password must be at least %d characters", minPasswordLen))
		return
	}

	exists, err := model.CheckUserExistOrDeleted(req.Email, req.Email)
	if err != nil {
		common.SysLog(fmt.Sprintf("modelhub register: CheckUserExistOrDeleted: %v", err))
		modelhubErr(c, http.StatusInternalServerError, "internal", "registration check failed")
		return
	}
	if exists {
		modelhubErr(c, http.StatusConflict, "user_exists", "an account with this email already exists")
		return
	}

	newUser := model.User{
		Username:    req.Email,
		Password:    req.Password,
		DisplayName: req.Email,
		Email:       req.Email,
		Role:        common.RoleCommonUser,
	}
	if err := newUser.Insert(0); err != nil {
		common.SysLog(fmt.Sprintf("modelhub register: Insert: %v", err))
		modelhubErr(c, http.StatusInternalServerError, "registration_failed", err.Error())
		return
	}

	var inserted model.User
	if err := model.DB.Where("username = ?", req.Email).First(&inserted).Error; err != nil {
		modelhubErr(c, http.StatusInternalServerError, "registration_followup_failed", "could not load newly created user")
		return
	}

	// S11: provision wallet account. No-op when wallet not wired (SQLite dev).
	ensureWalletAccountForUser(c.Request.Context(), inserted.Id)

	if err := modelhubSetupSession(c, &inserted); err != nil {
		modelhubErr(c, http.StatusInternalServerError, "session_save_failed", err.Error())
		return
	}
	respondAuthSuccess(c, &inserted)
}

// ModelhubLogin handles POST /v1/auth/login. Username for the inherited
// ValidateAndFill is the email (matches Register above).
func ModelhubLogin(c *gin.Context) {
	if !common.PasswordLoginEnabled {
		modelhubErr(c, http.StatusForbidden, "login_disabled", "password login is currently disabled")
		return
	}

	req, ok := decodeAuthBody(c)
	if !ok {
		return
	}

	user := model.User{Username: req.Email, Password: req.Password}
	if err := user.ValidateAndFill(); err != nil {
		// Deliberately collapse "no such user" + "wrong password" to one
		// message to avoid leaking which emails are registered.
		modelhubErr(c, http.StatusUnauthorized, "invalid_credentials", "invalid email or password")
		return
	}
	// Sprint 1 doesn't ship a 2FA flow in the modelhub SPA. Inherited
	// /api/user/login still handles 2FA for the admin UI — both paths
	// share the same model.User row, so a 2FA-protected account can keep
	// using the admin UI; the modelhub bridge just doesn't surface the
	// 2FA prompt yet.
	if err := modelhubSetupSession(c, &user); err != nil {
		modelhubErr(c, http.StatusInternalServerError, "session_save_failed", err.Error())
		return
	}
	respondAuthSuccess(c, &user)
}

// ModelhubLogout handles POST /v1/auth/logout. No auth required — clearing
// a non-existent session is a no-op.
func ModelhubLogout(c *gin.Context) {
	session := sessions.Default(c)
	session.Clear()
	if err := session.Save(); err != nil {
		modelhubErr(c, http.StatusInternalServerError, "session_save_failed", err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    gin.H{"logged_out": true},
	})
}

// decodeAuthBody parses + normalizes the email/password body. Writes a 400
// response and returns ok=false on malformed input.
func decodeAuthBody(c *gin.Context) (modelhubAuthRequest, bool) {
	var req modelhubAuthRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		modelhubErr(c, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return req, false
	}
	req.Email = strings.TrimSpace(strings.ToLower(req.Email))
	if req.Email == "" || req.Password == "" {
		modelhubErr(c, http.StatusBadRequest, "invalid_request", "email and password required")
		return req, false
	}
	return req, true
}

// modelhubSetupSession mirrors controller.setupLogin (user.go:98-112) so
// middleware.UserAuth picks up the same session keys regardless of which
// login path the user took.
func modelhubSetupSession(c *gin.Context, user *model.User) error {
	session := sessions.Default(c)
	session.Set("id", user.Id)
	session.Set("username", user.Username)
	session.Set("role", user.Role)
	session.Set("status", user.Status)
	session.Set("group", user.Group)
	return session.Save()
}

// respondAuthSuccess writes the canonical modelhub auth envelope. Used by
// register + login so /v1/auth/me and /v1/auth/{register,login} all return
// the same shape (frontend treats them interchangeably for the "current
// user" view).
func respondAuthSuccess(c *gin.Context, user *model.User) {
	accountID := wallet.UserAccountID(strconv.Itoa(user.Id))
	balance := readBalanceMicroUSD(c.Request.Context(), accountID)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data": gin.H{
			"user_id":           user.Id,
			"account_id":        string(accountID),
			"email":             user.Email,
			"username":          user.Username,
			"role":              user.Role,
			"balance_micro_usd": int64(balance),
		},
	})
}

// modelhubErr writes the canonical modelhub error envelope.
func modelhubErr(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{
		"success": false,
		"error":   gin.H{"code": code, "message": message},
	})
}

// ModelhubUserAuth is the SPA-friendly auth middleware for /v1/* routes.
//
// Why this instead of middleware.UserAuth:
//   The inherited middleware.UserAuth (middleware/auth.go:160) is a double-
//   factor check designed for the legacy admin UI: it requires BOTH a valid
//   session cookie AND a "New-Api-User: <user-id>" request header that
//   matches the session's user id. The modelhub SPA (modelhub-client.js)
//   is intentionally cookie-only — withCredentials:true and no custom
//   header — so it would always fail the second factor.
//
//   ModelhubUserAuth keeps the same session keys (id/username/role/group/
//   status) that the SPA-facing handlers read via c.GetInt etc., so handler
//   code stays identical regardless of which auth path mounted it.
func ModelhubUserAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		session := sessions.Default(c)
		id := session.Get("id")
		if id == nil {
			modelhubErr(c, http.StatusUnauthorized, "unauthenticated", "no session")
			c.Abort()
			return
		}
		status := session.Get("status")
		if status != nil {
			if s, ok := status.(int); ok && s == common.UserStatusDisabled {
				modelhubErr(c, http.StatusForbidden, "user_disabled", "user account is disabled")
				c.Abort()
				return
			}
		}
		c.Set("id", id)
		c.Set("username", session.Get("username"))
		c.Set("role", session.Get("role"))
		c.Set("group", session.Get("group"))
		c.Set("user_group", session.Get("group"))
		c.Next()
	}
}
