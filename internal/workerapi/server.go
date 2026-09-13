package workerapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/executionqueue"
	"github.com/TH1NKING/Build-your-own-Docker-with-Go/internal/workercredential"
)

type handler struct {
	queue   *executionqueue.Store
	slots   chan struct{}
	reports chan struct{}
}

func NewHandler(queue *executionqueue.Store) http.Handler {
	return &handler{queue: queue, slots: make(chan struct{}, 64), reports: make(chan struct{}, 1)}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path != "/worker/v1/claim" && r.URL.Path != "/worker/v1/validate" && r.URL.Path != "/worker/v1/complete" {
		http.NotFound(w, r)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		http.Error(w, "Worker API busy", http.StatusTooManyRequests)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" || token == r.Header.Get("Authorization") || len(token) > 256 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/worker/v1/validate" {
		var request leaseReference
		if !decode(w, r, &request, 1024) {
			http.Error(w, "invalid lease reference", http.StatusBadRequest)
			return
		}
		if err := h.queue.Validate(ctx, token, request.ExecutionID, request.Generation); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/worker/v1/complete" {
		// Authenticate before allocating a large result; the transaction checks
		// again after body decode to close the revocation race.
		if err := h.queue.Authenticate(ctx, token); err != nil {
			writeError(w, err)
			return
		}
		select {
		case h.reports <- struct{}{}:
			defer func() { <-h.reports }()
		default:
			http.Error(w, "report capacity unavailable", http.StatusTooManyRequests)
			return
		}
		var request completeRequest
		if !decodeComplete(w, r, &request) {
			http.Error(w, "invalid bounded result", http.StatusBadRequest)
			return
		}
		if err := h.queue.Complete(ctx, token, request.ExecutionID, request.Generation, request.Result); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var request claimRequest
	if !decode(w, r, &request, 1024) || request.WaitMilliseconds < 0 || request.WaitMilliseconds > 25000 {
		http.Error(w, "invalid claim", http.StatusBadRequest)
		return
	}
	until := time.Now().Add(time.Duration(request.WaitMilliseconds) * time.Millisecond)
	for {
		lease, err := h.queue.Claim(ctx, token)
		if err != nil {
			writeError(w, err)
			return
		}
		if lease != nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(lease)
			return
		}
		left := time.Until(until)
		if left <= 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		delay := min(left, 100*time.Millisecond)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func decode(w http.ResponseWriter, r *http.Request, value any, limit int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	err := object(d, func(key string) error {
		switch request := value.(type) {
		case *claimRequest:
			if key == "wait_ms" {
				return d.Decode(&request.WaitMilliseconds)
			}
		case *leaseReference:
			if key == "execution_id" {
				return d.Decode(&request.ExecutionID)
			}
			if key == "generation" {
				return d.Decode(&request.Generation)
			}
		}
		return executionqueue.ErrInvalid
	})
	if err != nil {
		return false
	}
	var extra any
	return d.Decode(&extra) == io.EOF
}
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, workercredential.ErrUnauthenticated):
		status = http.StatusUnauthorized
	case errors.Is(err, executionqueue.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, executionqueue.ErrLease), errors.Is(err, executionqueue.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, executionqueue.ErrNotFound):
		status = http.StatusNotFound
	}
	http.Error(w, http.StatusText(status), status)
}
