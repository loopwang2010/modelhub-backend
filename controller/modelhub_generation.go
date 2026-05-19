package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	modelhubapi "github.com/QuantumNous/new-api/internal/api"
	"github.com/QuantumNous/new-api/internal/catalog"
	"github.com/QuantumNous/new-api/internal/wallet"

	"github.com/gin-gonic/gin"
)

type generationCacheEntry struct {
	UserID   int
	Response *modelhubapi.GenerationResponse
	StoredAt time.Time
}

var generationCache = struct {
	sync.RWMutex
	byID   map[string]generationCacheEntry
	byIdem map[string]string
}{
	byID:   make(map[string]generationCacheEntry),
	byIdem: make(map[string]string),
}

const (
	generationCacheMaxEntries = 1000
	generationCacheTTL        = 24 * time.Hour
	generationSubmitTimeout   = 30 * time.Minute
)

type modelhubGenerationJob struct {
	UserID         int
	ID             string
	Model          adapter.ModelKey
	Manifest       catalog.ModelManifest
	Provider       adapter.ProviderAdapter
	Params         adapter.Params
	IdempotencyKey adapter.IdempotencyKey
	DedupKey       adapter.IdempotencyKey
	Credits        modelhubapi.Credits
	EscrowID       string
	Cost           adapter.CostUSD
	CreatedAt      time.Time
}

// ModelhubListModels exposes the modelhub catalog on a non-conflicting path.
func ModelhubListModels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"data": catalog.ListEnabled()})
}

// ModelhubSubmitGeneration accepts a generation and completes it in a
// background goroutine. The in-memory cache is the MVP task store; a durable
// DB-backed task worker should replace this before multi-node production.
func ModelhubSubmitGeneration(c *gin.Context) {
	userID := c.GetInt("id")
	if userID == 0 {
		modelhubErr(c, http.StatusUnauthorized, "unauthenticated", "no session")
		return
	}

	var req modelhubapi.GenerationRequest
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		modelhubErr(c, http.StatusBadRequest, "invalid_json", "request body is not valid JSON")
		return
	}
	if err := req.Validate(); err != nil {
		modelhubErr(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	manifest, err := catalog.Get(req.Model)
	if err != nil || !manifest.Enabled {
		modelhubErr(c, http.StatusNotFound, "model_not_found", "model is not available")
		return
	}
	if manifest.TaskKind != adapter.TaskKindSync {
		modelhubErr(c, http.StatusNotImplemented, "task_kind_not_wired", "only sync modelhub generations are wired in this build")
		return
	}

	provider, ok := adapter.Get(manifest.Provider)
	if !ok {
		modelhubErr(c, http.StatusServiceUnavailable, "provider_unavailable", fmt.Sprintf("provider %q is not configured", manifest.Provider))
		return
	}

	params, err := decodeGenerationParams(req.Params)
	if err != nil {
		modelhubErr(c, http.StatusBadRequest, "invalid_params", err.Error())
		return
	}
	if validator, ok := provider.(adapter.ParamsValidator); ok {
		if err := validator.Validate(req.Model, params); err != nil {
			modelhubErr(c, http.StatusBadRequest, "invalid_params", err.Error())
			return
		}
	}

	cost, err := provider.EstimateCost(req.Model, params)
	if err != nil {
		status, code := statusForPreSubmitError(err)
		modelhubErr(c, status, code, err.Error())
		return
	}

	accountID := wallet.UserAccountID(strconv.Itoa(userID))
	dedupKey := adapter.IdempotencyKey(strings.TrimSpace(req.IdempotencyKey))
	if dedupKey != "" {
		if existing, ok := cachedGenerationByIdem(userID, dedupKey); ok {
			c.JSON(statusCodeForGeneration(existing), existing)
			return
		}
	}

	genID, err := mintGenerationID()
	if err != nil {
		modelhubErr(c, http.StatusInternalServerError, "internal_error", "failed to mint generation id")
		return
	}
	idempotencyKey := dedupKey
	if idempotencyKey == "" {
		idempotencyKey = adapter.IdempotencyKey(genID)
	}

	credits := modelhubapi.Credits{}
	var escrowID string
	if w := getWallet(); w != nil && cost > 0 {
		if err := w.EnsureAccount(c.Request.Context(), accountID, wallet.AccountKindUserWallet, strconv.Itoa(userID)); err != nil {
			common.SysLog(fmt.Sprintf("modelhub generation: EnsureAccount(%s) failed: %v", accountID, err))
			modelhubErr(c, http.StatusInternalServerError, "wallet_error", "wallet account setup failed")
			return
		}
		escrowID, err = w.Hold(c.Request.Context(), accountID, genID, cost, idempotencyKey)
		if err != nil {
			status, code := statusForWalletError(err)
			modelhubErr(c, status, code, err.Error())
			return
		}
		credits.Held = cost
	}

	createdAt := time.Now().UTC()
	resp := queuedGenerationResponse(genID, req.Model, &manifest, credits, createdAt)
	cacheGenerationWithIdem(userID, dedupKey, resp)

	go runModelhubGeneration(modelhubGenerationJob{
		UserID:         userID,
		ID:             genID,
		Model:          req.Model,
		Manifest:       manifest,
		Provider:       provider,
		Params:         params,
		IdempotencyKey: idempotencyKey,
		DedupKey:       dedupKey,
		Credits:        credits,
		EscrowID:       escrowID,
		Cost:           cost,
		CreatedAt:      createdAt,
	})

	c.JSON(http.StatusAccepted, resp)
}

func runModelhubGeneration(job modelhubGenerationJob) {
	cacheGenerationWithIdem(job.UserID, job.DedupKey, runningGenerationResponse(job))

	ctx, cancel := context.WithTimeout(context.Background(), generationSubmitTimeout)
	defer cancel()

	result, err := job.Provider.Submit(ctx, job.Model, job.Params, job.IdempotencyKey)
	if err != nil {
		refundGenerationEscrow(ctx, job.ID, job.EscrowID, &job.Credits)
		cacheGenerationWithIdem(job.UserID, job.DedupKey, failedGenerationResponseAt(job.ID, job.Model, &job.Manifest, job.Credits, err, job.CreatedAt))
		return
	}

	syncResult, ok := result.(adapter.SyncSubmit)
	if !ok {
		if ptr, ptrOK := result.(*adapter.SyncSubmit); ptrOK && ptr != nil {
			syncResult = *ptr
			ok = true
		}
	}
	if !ok {
		refundGenerationEscrow(ctx, job.ID, job.EscrowID, &job.Credits)
		resp := failedGenerationResponseAt(job.ID, job.Model, &job.Manifest, job.Credits, errors.New("provider returned non-sync result for sync manifest"), job.CreatedAt)
		cacheGenerationWithIdem(job.UserID, job.DedupKey, resp)
		return
	}

	if job.EscrowID != "" {
		if err := getWallet().Settle(ctx, job.EscrowID, job.Cost); err != nil && !errors.Is(err, wallet.ErrEscrowAlreadySettled) {
			common.SysLog(fmt.Sprintf("modelhub generation: settle failed for %s: %v", job.ID, err))
			resp := failedGenerationResponseAt(job.ID, job.Model, &job.Manifest, job.Credits, err, job.CreatedAt)
			cacheGenerationWithIdem(job.UserID, job.DedupKey, resp)
			return
		}
		job.Credits.Settled = job.Cost
	}

	resp, err := succeededGenerationResponseAt(job.ID, job.Model, &job.Manifest, job.Credits, syncResult.Result, job.CreatedAt)
	if err != nil {
		resp = failedGenerationResponseAt(job.ID, job.Model, &job.Manifest, job.Credits, err, job.CreatedAt)
	}
	cacheGenerationWithIdem(job.UserID, job.DedupKey, resp)
}

// ModelhubGetGeneration serves recent sync responses from the in-memory cache.
func ModelhubGetGeneration(c *gin.Context) {
	userID := c.GetInt("id")
	if userID == 0 {
		modelhubErr(c, http.StatusUnauthorized, "unauthenticated", "no session")
		return
	}
	id := c.Param("id")
	resp, ok := cachedGeneration(userID, id)
	if !ok {
		modelhubErr(c, http.StatusNotFound, "generation_not_found", "generation was not found in this process")
		return
	}
	c.JSON(http.StatusOK, resp)
}

func decodeGenerationParams(raw json.RawMessage) (adapter.Params, error) {
	var params adapter.Params
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, fmt.Errorf("params must be a JSON object: %w", err)
	}
	if params == nil {
		return nil, errors.New("params must be a JSON object")
	}
	return params, nil
}

func mintGenerationID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "gen_" + hex.EncodeToString(buf[:]), nil
}

func queuedGenerationResponse(id string, model adapter.ModelKey, manifest *catalog.ModelManifest, credits modelhubapi.Credits, createdAt time.Time) *modelhubapi.GenerationResponse {
	return &modelhubapi.GenerationResponse{
		ID:        id,
		Model:     model,
		Status:    modelhubapi.StatusQueued,
		Modality:  manifest.Modality,
		TaskKind:  manifest.TaskKind,
		CreatedAt: createdAt,
		Credits:   credits,
	}
}

func runningGenerationResponse(job modelhubGenerationJob) *modelhubapi.GenerationResponse {
	return &modelhubapi.GenerationResponse{
		ID:        job.ID,
		Model:     job.Model,
		Status:    modelhubapi.StatusRunning,
		Modality:  job.Manifest.Modality,
		TaskKind:  job.Manifest.TaskKind,
		CreatedAt: job.CreatedAt,
		Credits:   job.Credits,
	}
}

func succeededGenerationResponse(id string, model adapter.ModelKey, manifest *catalog.ModelManifest, credits modelhubapi.Credits, result *adapter.NormalizedResult) (*modelhubapi.GenerationResponse, error) {
	return succeededGenerationResponseAt(id, model, manifest, credits, result, time.Now().UTC())
}

func succeededGenerationResponseAt(id string, model adapter.ModelKey, manifest *catalog.ModelManifest, credits modelhubapi.Credits, result *adapter.NormalizedResult, createdAt time.Time) (*modelhubapi.GenerationResponse, error) {
	now := time.Now().UTC()
	output, err := outputFromNormalizedResult(result)
	if err != nil {
		return nil, err
	}
	return &modelhubapi.GenerationResponse{
		ID:          id,
		Model:       model,
		Status:      modelhubapi.StatusSucceeded,
		Modality:    manifest.Modality,
		TaskKind:    manifest.TaskKind,
		CreatedAt:   createdAt,
		CompletedAt: &now,
		Output:      output,
		Credits:     credits,
	}, nil
}

func failedGenerationResponse(id string, model adapter.ModelKey, manifest *catalog.ModelManifest, credits modelhubapi.Credits, err error) *modelhubapi.GenerationResponse {
	return failedGenerationResponseAt(id, model, manifest, credits, err, time.Now().UTC())
}

func failedGenerationResponseAt(id string, model adapter.ModelKey, manifest *catalog.ModelManifest, credits modelhubapi.Credits, err error, createdAt time.Time) *modelhubapi.GenerationResponse {
	now := time.Now().UTC()
	class := adapterErrorClass(err)
	return &modelhubapi.GenerationResponse{
		ID:          id,
		Model:       model,
		Status:      modelhubapi.StatusFailed,
		Modality:    manifest.Modality,
		TaskKind:    manifest.TaskKind,
		CreatedAt:   createdAt,
		CompletedAt: &now,
		Error: &modelhubapi.Error{
			Code:    string(class),
			Message: err.Error(),
		},
		Credits: credits,
	}
}

func refundGenerationEscrow(ctx context.Context, generationID string, escrowID string, credits *modelhubapi.Credits) {
	if escrowID == "" {
		return
	}
	w := getWallet()
	if w == nil {
		return
	}
	if err := w.Refund(ctx, escrowID); err != nil && !errors.Is(err, wallet.ErrEscrowAlreadySettled) {
		common.SysLog(fmt.Sprintf("modelhub generation: refund failed for %s: %v", generationID, err))
		return
	}
	credits.Refunded = credits.Held
}

func outputFromNormalizedResult(result *adapter.NormalizedResult) (*modelhubapi.Output, error) {
	if result == nil || len(result.Outputs) == 0 {
		return nil, errors.New("generation returned no output")
	}
	first := result.Outputs[0]
	out := &modelhubapi.Output{
		Type:      first.Kind,
		MimeType:  first.MimeType,
		SizeBytes: first.SizeBytes,
		Metadata:  result.Metadata,
	}
	switch first.Kind {
	case adapter.OutputKindImageURL, adapter.OutputKindVideoURL, adapter.OutputKindAudioURL:
		if first.URL == "" {
			return nil, errors.New("generation output missing URL")
		}
		out.URL = first.URL
	case adapter.OutputKindText:
		if text, ok := result.Metadata["text"].(string); ok {
			out.Text = text
		}
	case adapter.OutputKindBase64:
		if b64, ok := result.Metadata["base64"].(string); ok {
			out.Base64 = b64
		}
	default:
		return nil, fmt.Errorf("unsupported output type %q", first.Kind)
	}
	return out, nil
}

type adapterClassedError interface {
	ErrorClass() adapter.ErrorClass
}

func adapterErrorClass(err error) adapter.ErrorClass {
	var classed adapterClassedError
	if errors.As(err, &classed) && classed.ErrorClass() != "" {
		return classed.ErrorClass()
	}
	if errors.Is(err, adapter.ErrInvalidParams) {
		return adapter.ErrClassUnknown
	}
	if errors.Is(err, adapter.ErrNotConfigured) {
		return adapter.ErrClassAuth
	}
	return adapter.ErrClassUpstream
}

func statusForPreSubmitError(err error) (int, string) {
	switch {
	case errors.Is(err, adapter.ErrInvalidParams):
		return http.StatusBadRequest, "invalid_params"
	case errors.Is(err, adapter.ErrCostCeilingExceeded):
		return http.StatusBadRequest, "cost_ceiling_exceeded"
	case errors.Is(err, adapter.ErrNotConfigured):
		return http.StatusServiceUnavailable, "provider_unavailable"
	default:
		return http.StatusInternalServerError, "cost_estimate_failed"
	}
}

func statusForWalletError(err error) (int, string) {
	switch {
	case errors.Is(err, wallet.ErrInsufficientBalance):
		return http.StatusPaymentRequired, "insufficient_balance"
	case errors.Is(err, wallet.ErrCostCeilingExceeded):
		return http.StatusBadRequest, "cost_ceiling_exceeded"
	default:
		return http.StatusInternalServerError, "wallet_error"
	}
}

func cacheGeneration(userID int, resp *modelhubapi.GenerationResponse) {
	cacheGenerationWithIdem(userID, "", resp)
}

func cacheGenerationWithIdem(userID int, idem adapter.IdempotencyKey, resp *modelhubapi.GenerationResponse) {
	if resp == nil || resp.ID == "" {
		return
	}
	now := time.Now().UTC()
	generationCache.Lock()
	defer generationCache.Unlock()
	if len(generationCache.byID) >= generationCacheMaxEntries {
		pruneGenerationCacheLocked(now)
	}
	generationCache.byID[resp.ID] = generationCacheEntry{
		UserID:   userID,
		Response: resp,
		StoredAt: now,
	}
	if idem != "" {
		generationCache.byIdem[generationIdemCacheKey(userID, idem)] = resp.ID
	}
}

func cachedGeneration(userID int, id string) (*modelhubapi.GenerationResponse, bool) {
	now := time.Now().UTC()
	generationCache.RLock()
	entry, ok := generationCache.byID[id]
	generationCache.RUnlock()
	if !ok || entry.UserID != userID {
		return nil, false
	}
	if now.Sub(entry.StoredAt) > generationCacheTTL {
		generationCache.Lock()
		delete(generationCache.byID, id)
		generationCache.Unlock()
		return nil, false
	}
	return entry.Response, true
}

func cachedGenerationByIdem(userID int, idem adapter.IdempotencyKey) (*modelhubapi.GenerationResponse, bool) {
	if idem == "" {
		return nil, false
	}
	generationCache.RLock()
	id, ok := generationCache.byIdem[generationIdemCacheKey(userID, idem)]
	generationCache.RUnlock()
	if !ok {
		return nil, false
	}
	return cachedGeneration(userID, id)
}

func generationIdemCacheKey(userID int, idem adapter.IdempotencyKey) string {
	return fmt.Sprintf("%d:%s", userID, idem)
}

func statusCodeForGeneration(resp *modelhubapi.GenerationResponse) int {
	if resp == nil {
		return http.StatusOK
	}
	switch resp.Status {
	case modelhubapi.StatusQueued, modelhubapi.StatusRunning:
		return http.StatusAccepted
	default:
		return http.StatusOK
	}
}

func pruneGenerationCacheLocked(now time.Time) {
	for id, entry := range generationCache.byID {
		if now.Sub(entry.StoredAt) > generationCacheTTL {
			delete(generationCache.byID, id)
		}
	}
	for key, id := range generationCache.byIdem {
		if _, ok := generationCache.byID[id]; !ok {
			delete(generationCache.byIdem, key)
		}
	}
	if len(generationCache.byID) < generationCacheMaxEntries {
		return
	}
	for id := range generationCache.byID {
		delete(generationCache.byID, id)
		for key, mappedID := range generationCache.byIdem {
			if mappedID == id {
				delete(generationCache.byIdem, key)
			}
		}
		if len(generationCache.byID) < generationCacheMaxEntries {
			return
		}
	}
}
