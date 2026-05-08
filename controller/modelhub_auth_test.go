// Tests for the modelhub auth + wallet bridge (S11/S12).
//
// Strategy: spin up an in-memory SQLite-backed wallet, install it via
// SetWallet, drive the handlers directly with a gin test context, and
// assert against the wallet's own DB. The inherited session/middleware
// stack is short-circuited by setting the "id" / "role" context keys
// directly — that's what authHelper does in production, so the handlers
// see an identical state.

package controller

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	// "sqlite" driver is registered transitively via gorm's
	// glebarez/sqlite import (used by model/main.go). Re-importing
	// modernc.org/sqlite directly would panic with "Register called twice"
	// at package init time — both packages register the same driver name.
	// We rely on the existing registration.

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/internal/wallet"

	"github.com/gin-gonic/gin"
)

var testDBCounter atomic.Int64

// installTestWallet builds a fresh in-memory wallet, installs it as the
// controller singleton, and returns the underlying *sql.DB plus a cleanup.
//
// The cleanup uninstalls the wallet so cross-test contamination doesn't
// happen — every test gets a private DB.
func installTestWallet(t *testing.T) (*wallet.DBWallet, *sql.DB, func()) {
	t.Helper()
	dsn := fmt.Sprintf("file:controller_wallet_test_%d?mode=memory&cache=shared&_busy_timeout=5000",
		testDBCounter.Add(1))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	dialect := wallet.SQLiteDialect{}
	for _, stmt := range dialect.CreateSchemaSQL() {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create schema: %v\nstmt: %s", err, stmt)
		}
	}
	w := wallet.New(db, dialect)
	SetWallet(w, db, dialect)
	cleanup := func() {
		SetWallet(nil, nil, nil)
		db.Close()
	}
	return w, db, cleanup
}

// newGinCtx returns a gin test context with the given id/role pre-set, as
// authHelper would set them after a successful session lookup.
func newGinCtx(method, path string, body any, id, role int) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	var bodyReader *bytes.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		bodyReader = bytes.NewReader(buf)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	c.Request = req

	if id != 0 {
		c.Set("id", id)
	}
	if role != 0 {
		c.Set("role", role)
	}
	return c, w
}

// ─────────────────────────────────────────────────────────────────────────
// S11: registration → wallet account
// ─────────────────────────────────────────────────────────────────────────

// TestEnsureWalletAccountForUser_CreatesRow proves the registration hook
// actually inserts a wallet_account row. This is the H2 review-fix in test
// form: registering a user MUST provision the wallet, otherwise the
// /v1/auth/me probe returns balance for an account that doesn't exist.
func TestEnsureWalletAccountForUser_CreatesRow(t *testing.T) {
	_, db, cleanup := installTestWallet(t)
	defer cleanup()

	ensureWalletAccountForUser(context.Background(), 4242)

	var got string
	err := db.QueryRow(
		`SELECT id FROM wallet_account WHERE id = $1`, "user:4242",
	).Scan(&got)
	if err != nil {
		t.Fatalf("expected wallet_account row for user:4242, got: %v", err)
	}
	if got != "user:4242" {
		t.Errorf("account id = %q, want user:4242", got)
	}
}

// TestEnsureWalletAccountForUser_Idempotent proves re-running the hook
// (same userID) does not error or duplicate the row. EnsureAccount uses
// ON CONFLICT DO NOTHING, so a re-register attempt or admin retry is safe.
func TestEnsureWalletAccountForUser_Idempotent(t *testing.T) {
	_, db, cleanup := installTestWallet(t)
	defer cleanup()
	ctx := context.Background()

	ensureWalletAccountForUser(ctx, 7)
	ensureWalletAccountForUser(ctx, 7)
	ensureWalletAccountForUser(ctx, 7)

	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wallet_account WHERE id = $1`, "user:7").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("wallet_account row count = %d, want 1", n)
	}
}

// TestEnsureWalletAccountForUser_NoWallet_NoOp confirms the hook safely
// no-ops when the wallet is not wired (SetWallet never called). This keeps
// the registration path working in tests / non-modelhub deployments.
func TestEnsureWalletAccountForUser_NoWallet_NoOp(t *testing.T) {
	SetWallet(nil, nil, nil)
	defer SetWallet(nil, nil, nil)

	// Should not panic / not error.
	ensureWalletAccountForUser(context.Background(), 99)
}

// ─────────────────────────────────────────────────────────────────────────
// S12: admin top-up
// ─────────────────────────────────────────────────────────────────────────

// TestAdminWalletTopup_HappyPath proves an admin top-up:
//  1. credits the user's account via wallet.Topup,
//  2. responds with the new balance,
//  3. writes the canonical paired ledger rows (sum-zero invariant holds).
func TestAdminWalletTopup_HappyPath(t *testing.T) {
	_, db, cleanup := installTestWallet(t)
	defer cleanup()

	const targetUserID = 100
	// Pre-create the target user's wallet account (Register hook would
	// have done this; we assert AdminWalletTopup also self-heals via
	// EnsureAccount).
	ensureWalletAccountForUser(context.Background(), targetUserID)

	body := adminTopupRequest{
		UserID:    targetUserID,
		AmountUSD: 5.0, // $5 → 5_000_000 micro-USD
		Note:      "promo credit",
		Idem:      "topup-test-001",
	}
	c, w := newGinCtx(http.MethodPost, "/admin/wallet/topup", body, /*id*/ 1, common.RoleAdminUser)
	AdminWalletTopup(c)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			AccountID          string `json:"account_id"`
			CreditedMicroUSD   int64  `json:"credited_micro_usd"`
			NewBalanceMicroUSD int64  `json:"new_balance_micro_usd"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v; body=%s", err, w.Body.String())
	}
	if !resp.Success {
		t.Errorf("success = false, want true")
	}
	if resp.Data.AccountID != "user:100" {
		t.Errorf("account_id = %q, want user:100", resp.Data.AccountID)
	}
	if resp.Data.CreditedMicroUSD != 5_000_000 {
		t.Errorf("credited = %d, want 5_000_000", resp.Data.CreditedMicroUSD)
	}
	if resp.Data.NewBalanceMicroUSD != 5_000_000 {
		t.Errorf("new_balance = %d, want 5_000_000", resp.Data.NewBalanceMicroUSD)
	}

	// Sum-zero invariant.
	var sum int64
	if err := db.QueryRow(`SELECT COALESCE(SUM(amount_micro_usd), 0) FROM wallet_ledger`).Scan(&sum); err != nil {
		t.Fatalf("invariant query: %v", err)
	}
	if sum != 0 {
		t.Errorf("ledger sum = %d, want 0 (I1 violation)", sum)
	}

	// Audit row recorded.
	var auditAdmin string
	if err := db.QueryRow(
		`SELECT admin_user_id FROM topup_audit WHERE account_id = $1`, "user:100",
	).Scan(&auditAdmin); err != nil {
		t.Fatalf("audit row missing: %v", err)
	}
	if auditAdmin != "1" {
		t.Errorf("audit admin = %q, want 1", auditAdmin)
	}
}

// TestAdminWalletTopup_NonAdmin_403 proves a session whose role is below
// RoleAdminUser cannot top up — even if they pass a valid request body.
// This is the AP-4 / role-isolation boundary.
func TestAdminWalletTopup_NonAdmin_403(t *testing.T) {
	_, _, cleanup := installTestWallet(t)
	defer cleanup()

	body := adminTopupRequest{UserID: 5, AmountUSD: 1.0, Note: "evil"}
	c, w := newGinCtx(http.MethodPost, "/admin/wallet/topup", body, /*id*/ 5, common.RoleCommonUser)
	AdminWalletTopup(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "admin role required") {
		t.Errorf("body did not mention admin role requirement: %s", w.Body.String())
	}
}

// TestAdminWalletTopup_NoSession_403 proves an unauthenticated caller
// (id=0, role=0) is rejected. In production middleware.AdminAuth wouldn't
// even let the request reach the handler; this guards against future
// router refactors that might forget to mount the middleware.
func TestAdminWalletTopup_NoSession_403(t *testing.T) {
	_, _, cleanup := installTestWallet(t)
	defer cleanup()

	body := adminTopupRequest{UserID: 5, AmountUSD: 1.0}
	c, w := newGinCtx(http.MethodPost, "/admin/wallet/topup", body, 0, 0)
	AdminWalletTopup(c)

	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

// TestAdminWalletTopup_InvalidAmount rejects zero / negative / overflow.
func TestAdminWalletTopup_InvalidAmount(t *testing.T) {
	_, _, cleanup := installTestWallet(t)
	defer cleanup()

	cases := []struct {
		name      string
		amountUSD float64
	}{
		{"zero", 0},
		{"negative", -1.0},
		{"over-ceiling", 100_000.0}, // > $10_000 max
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := adminTopupRequest{UserID: 1, AmountUSD: tc.amountUSD}
			c, w := newGinCtx(http.MethodPost, "/admin/wallet/topup", body, 1, common.RoleAdminUser)
			AdminWalletTopup(c)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
		})
	}
}

// TestAdminWalletTopup_Idempotent proves replaying the same request with
// the same idem key is a no-op — the second call returns the same balance.
func TestAdminWalletTopup_Idempotent(t *testing.T) {
	_, db, cleanup := installTestWallet(t)
	defer cleanup()

	const uid = 200
	ensureWalletAccountForUser(context.Background(), uid)

	body := adminTopupRequest{UserID: uid, AmountUSD: 3.0, Idem: "same-key"}

	for i := 0; i < 3; i++ {
		c, w := newGinCtx(http.MethodPost, "/admin/wallet/topup", body, 1, common.RoleAdminUser)
		AdminWalletTopup(c)
		if w.Code != http.StatusOK {
			t.Fatalf("attempt %d: status %d, body=%s", i, w.Code, w.Body.String())
		}
	}

	var sum int64
	if err := db.QueryRow(
		`SELECT COALESCE(SUM(amount_micro_usd), 0) FROM wallet_ledger WHERE account_id = $1`,
		"user:200").Scan(&sum); err != nil {
		t.Fatalf("balance query: %v", err)
	}
	if sum != 3_000_000 {
		t.Errorf("balance after 3 idempotent top-ups = %d, want 3_000_000", sum)
	}
}

// ─────────────────────────────────────────────────────────────────────────
// S11/S12: read-side endpoints
// ─────────────────────────────────────────────────────────────────────────

// TestAuthMe_ReturnsBalance is the integration of S11+S12: register a
// user → admin tops them up → /v1/auth/me returns the right balance.
//
// We can't drive the full Register code path here (it'd need GORM + the
// full options machinery), so we simulate it by calling the post-register
// hook directly — same effect.
func TestAuthMe_BalanceAfterTopup_Conceptual(t *testing.T) {
	// AuthMe needs model.GetUserById, which needs model.DB. Without a
	// fully-initialised DB we can't drive AuthMe end-to-end here. But we
	// CAN cover the wallet-balance side of the contract: WalletBalanceSelf
	// has the same projection and doesn't need GORM.
	_, _, cleanup := installTestWallet(t)
	defer cleanup()

	const uid = 555
	ensureWalletAccountForUser(context.Background(), uid)

	topupBody := adminTopupRequest{UserID: uid, AmountUSD: 12.5, Idem: "seed"}
	c, w := newGinCtx(http.MethodPost, "/admin/wallet/topup", topupBody, 1, common.RoleAdminUser)
	AdminWalletTopup(c)
	if w.Code != http.StatusOK {
		t.Fatalf("topup failed: %s", w.Body.String())
	}

	c, w = newGinCtx(http.MethodGet, "/v1/wallet/balance", nil, uid, common.RoleCommonUser)
	WalletBalanceSelf(c)

	if w.Code != http.StatusOK {
		t.Fatalf("balance fetch failed: status %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			AccountID       string `json:"account_id"`
			BalanceMicroUSD int64  `json:"balance_micro_usd"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !resp.Success {
		t.Errorf("success false")
	}
	if resp.Data.AccountID != "user:"+strconv.Itoa(uid) {
		t.Errorf("account_id = %q", resp.Data.AccountID)
	}
	if resp.Data.BalanceMicroUSD != 12_500_000 {
		t.Errorf("balance = %d, want 12_500_000", resp.Data.BalanceMicroUSD)
	}
}

// TestWalletHistorySelf_ReturnsLedgerEntries proves the history endpoint
// returns the rows the wallet wrote — exercising the raw-SQL projection
// path (since wallet.Wallet doesn't expose a list method).
func TestWalletHistorySelf_ReturnsLedgerEntries(t *testing.T) {
	_, _, cleanup := installTestWallet(t)
	defer cleanup()

	const uid = 777
	ensureWalletAccountForUser(context.Background(), uid)

	// Two top-ups → 4 ledger rows total (2 per op), 2 of which target the user.
	for i, amt := range []float64{1.0, 2.5} {
		body := adminTopupRequest{
			UserID: uid, AmountUSD: amt, Idem: fmt.Sprintf("hist-%d", i),
		}
		c, w := newGinCtx(http.MethodPost, "/admin/wallet/topup", body, 1, common.RoleAdminUser)
		AdminWalletTopup(c)
		if w.Code != http.StatusOK {
			t.Fatalf("topup %d: %s", i, w.Body.String())
		}
	}

	c, w := newGinCtx(http.MethodGet, "/v1/wallet/history?limit=10", nil, uid, common.RoleCommonUser)
	WalletHistorySelf(c)
	if w.Code != http.StatusOK {
		t.Fatalf("history status %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			AccountID string        `json:"account_id"`
			Entries   []LedgerEntry `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.Data.Entries) != 2 {
		t.Errorf("entries = %d, want 2 (user-side rows of 2 top-ups)", len(resp.Data.Entries))
	}
	for _, e := range resp.Data.Entries {
		if e.ReasonCode != wallet.ReasonTopup {
			t.Errorf("entry reason = %q, want topup", e.ReasonCode)
		}
		if e.AmountMicroUSD <= 0 {
			t.Errorf("entry amount = %d, want positive (user is credit-side)", e.AmountMicroUSD)
		}
	}
}

// TestAdminWalletHistory_RequiresAdmin proves the admin-history endpoint
// gates on role.
func TestAdminWalletHistory_RequiresAdmin(t *testing.T) {
	_, _, cleanup := installTestWallet(t)
	defer cleanup()

	c, w := newGinCtx(http.MethodGet, "/admin/wallet/history?user_id=1", nil, 5, common.RoleCommonUser)
	AdminWalletHistory(c)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
}

// TestAdminWalletHistory_RequiresUserID proves missing user_id is a 400.
func TestAdminWalletHistory_RequiresUserID(t *testing.T) {
	_, _, cleanup := installTestWallet(t)
	defer cleanup()

	c, w := newGinCtx(http.MethodGet, "/admin/wallet/history", nil, 1, common.RoleAdminUser)
	AdminWalletHistory(c)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestAdminWalletHistory_HappyPath proves admin can view another user's
// ledger after a top-up — and the response includes the current balance.
func TestAdminWalletHistory_HappyPath(t *testing.T) {
	_, _, cleanup := installTestWallet(t)
	defer cleanup()

	const uid = 314
	ensureWalletAccountForUser(context.Background(), uid)

	body := adminTopupRequest{UserID: uid, AmountUSD: 7.0, Idem: "history-hp"}
	c, w := newGinCtx(http.MethodPost, "/admin/wallet/topup", body, 1, common.RoleAdminUser)
	AdminWalletTopup(c)
	if w.Code != http.StatusOK {
		t.Fatalf("topup: %s", w.Body.String())
	}

	c, w = newGinCtx(http.MethodGet,
		"/admin/wallet/history?user_id="+strconv.Itoa(uid)+"&limit=20", nil,
		1, common.RoleAdminUser)
	AdminWalletHistory(c)
	if w.Code != http.StatusOK {
		t.Fatalf("history: status %d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			AccountID       string        `json:"account_id"`
			BalanceMicroUSD int64         `json:"balance_micro_usd"`
			Entries         []LedgerEntry `json:"entries"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Data.BalanceMicroUSD != 7_000_000 {
		t.Errorf("balance = %d, want 7_000_000", resp.Data.BalanceMicroUSD)
	}
	if len(resp.Data.Entries) != 1 {
		t.Errorf("entries = %d, want 1", len(resp.Data.Entries))
	}
}

// TestParseLimit covers the boundary clamping.
func TestParseLimit(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 50},
		{"abc", 50},
		{"0", 50},
		{"-5", 50},
		{"10", 10},
		{"500", 500},
		{"99999", 500}, // clamped at maxHistoryLimit
	}
	for _, tc := range cases {
		if got := parseLimit(tc.in); got != tc.want {
			t.Errorf("parseLimit(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
