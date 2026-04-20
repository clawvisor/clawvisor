package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"

	"github.com/clawvisor/clawvisor/internal/api/middleware"
	"github.com/clawvisor/clawvisor/pkg/gateway"
)

// maxBatchSize caps how many sub-requests a single batch may carry. Chosen to
// bound worst-case fan-out (LLM calls, adapter HTTP calls) per round-trip.
const maxBatchSize = 20

// HandleBatch accepts N gateway requests and returns N results in a single
// round-trip. Each sub-request runs through the existing single-request
// pipeline (HandleRequest) — auth, restrictions, task scope, intent
// verification, audit — so behavior is identical to calling the single
// endpoint N times.
//
// Sub-requests execute concurrently, bounded by maxBatchSize. A failure in
// one sub-request never aborts the batch; each result carries its own
// status/code. Ordering of results matches the input ordering.
//
// POST /api/gateway/batch
// Auth: agent bearer token (same as single-request endpoint)
// Query params: wait=true&timeout=N are forwarded to each sub-request.
func (h *GatewayHandler) HandleBatch(w http.ResponseWriter, r *http.Request) {
	agent := middleware.AgentFromContext(r.Context())
	if agent == nil {
		writeError(w, http.StatusUnauthorized, gateway.CodeUnauthorized, "not authenticated")
		return
	}

	var batch gateway.BatchRequest
	if !decodeJSON(w, r, &batch) {
		return
	}

	if len(batch.Requests) == 0 {
		writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
			Error: "batch contains no requests",
			Code:  gateway.CodeBatchEmpty,
			Hint:  "Send at least one sub-request in the \"requests\" array.",
		})
		return
	}
	if len(batch.Requests) > maxBatchSize {
		writeDetailedError(w, http.StatusBadRequest, apiErrorDetail{
			Error: "batch exceeds the maximum size",
			Code:  gateway.CodeBatchTooLarge,
			Hint:  "Split the batch into smaller chunks. Each batch may contain at most " + strconv.Itoa(maxBatchSize) + " sub-requests.",
		})
		return
	}

	// Preserve caller-provided wait/timeout query params so each sub-request
	// inherits the same long-poll behavior as a direct call would.
	subQuery := ""
	if q := r.URL.RawQuery; q != "" {
		subQuery = "?" + q
	}

	results := make([]map[string]any, len(batch.Requests))
	var wg sync.WaitGroup
	for i := range batch.Requests {
		wg.Add(1)
		go func(idx int, sub gateway.Request) {
			defer wg.Done()
			// Per-goroutine recover: middleware.Recover wraps the outer
			// handler but cannot catch panics from child goroutines — an
			// unrecovered panic here would crash the whole process.
			defer func() {
				if rec := recover(); rec != nil {
					h.logger.Error("batch sub-request panicked",
						"panic", rec,
						"request_id", sub.RequestID,
						"service", sub.Service,
						"action", sub.Action,
					)
					results[idx] = map[string]any{
						"status":     "error",
						"request_id": sub.RequestID,
						"error":      "internal error processing sub-request",
						"code":       gateway.CodeInternalError,
					}
				}
			}()
			results[idx] = h.invokeSingle(r, sub, subQuery)
		}(i, batch.Requests[i])
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, gateway.BatchResponse{Results: results})
}

// invokeSingle dispatches one sub-request through HandleRequest using a
// synthetic http.Request that carries the original authenticated context.
// The response is captured, decoded as JSON, and returned as a generic map
// so it can be passed through the batch response verbatim.
func (h *GatewayHandler) invokeSingle(orig *http.Request, sub gateway.Request, query string) map[string]any {
	body, err := json.Marshal(sub)
	if err != nil {
		return map[string]any{
			"status":     "error",
			"request_id": sub.RequestID,
			"error":      "failed to re-encode sub-request: " + err.Error(),
			"code":       gateway.CodeInternalError,
		}
	}

	subReq, err := http.NewRequestWithContext(
		orig.Context(),
		http.MethodPost,
		"/api/gateway/request"+query,
		bytes.NewReader(body),
	)
	if err != nil {
		return map[string]any{
			"status":     "error",
			"request_id": sub.RequestID,
			"error":      err.Error(),
			"code":       gateway.CodeInternalError,
		}
	}
	subReq.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.HandleRequest(rec, subReq)

	var out map[string]any
	if dErr := json.Unmarshal(rec.Body.Bytes(), &out); dErr != nil {
		return map[string]any{
			"status":     "error",
			"request_id": sub.RequestID,
			"error":      "sub-response was not JSON: " + dErr.Error(),
			"code":       gateway.CodeInternalError,
		}
	}
	if _, ok := out["request_id"]; !ok && sub.RequestID != "" {
		out["request_id"] = sub.RequestID
	}
	return out
}

