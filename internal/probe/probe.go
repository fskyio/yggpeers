// Package probe runs an authenticated, outbound-only distributed probe.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"yggpeers/internal/checker"
	"yggpeers/internal/config"
	"yggpeers/internal/protocol"
)

const retryDelay = time.Second

var errRoundClosed = errors.New("probe round closed")

type apiError struct {
	status int
}

func (e *apiError) Error() string { return fmt.Sprintf("probe API returned HTTP %d", e.status) }

func permanent(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.status >= 400 && apiErr.status < 500 && apiErr.status != http.StatusRequestTimeout && apiErr.status != http.StatusTooManyRequests
}

type Worker struct {
	cfg     config.ProbeConfig
	checker *checker.Checker
	client  *http.Client
}

func New(cfg config.ProbeConfig) (*Worker, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.ServerURL = strings.TrimRight(cfg.ServerURL, "/")
	return &Worker{
		cfg:     cfg,
		checker: checker.New(cfg),
		client: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				TLSHandshakeTimeout: 10 * time.Second,
				IdleConnTimeout:     90 * time.Second,
				MaxIdleConns:        2,
			},
			// Never forward a credential to a redirect target or downgrade HTTPS.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return fmt.Errorf("probe API redirects are not allowed")
			},
		},
	}, nil
}

// Run checks in and executes assignments until cancellation or a permanent API
// rejection. Authentication alone selects the registered probe's identity.
func (w *Worker) Run(ctx context.Context) error {
	defer w.client.CloseIdleConnections()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var response protocol.WorkResponse
		err := w.post(ctx, protocol.CheckInPath, protocol.CheckInRequest{CheckDarknet: w.cfg.CheckDarknet}, &response)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if permanent(err) {
				return err
			}
			log.Printf("probe check-in: %v", err)
			if err := pause(ctx, retryDelay); err != nil {
				return err
			}
			continue
		}
		if response.Work != nil {
			if err := w.execute(ctx, *response.Work); err != nil {
				return err
			}
		}
		delay := 5 * time.Second
		// Bound the server's hint before multiplying to avoid duration overflow.
		if response.PollAfter > 0 {
			milliseconds := min(max(response.PollAfter, 100), 60000)
			delay = time.Duration(milliseconds) * time.Millisecond
		}
		if err := pause(ctx, delay); err != nil {
			return err
		}
	}
}

func (w *Worker) execute(ctx context.Context, assignment protocol.Assignment) error {
	if assignment.RoundID <= 0 || assignment.Deadline.IsZero() {
		return fmt.Errorf("probe API returned an invalid assignment")
	}
	if !time.Now().Before(assignment.Deadline) {
		return nil
	}
	jobCtx, cancel := context.WithDeadline(ctx, assignment.Deadline)
	defer cancel()
	for start := 0; start < len(assignment.Targets); {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if jobCtx.Err() != nil {
			return nil
		}
		if start > 0 && w.cfg.BatchDelay > 0 {
			if err := pause(jobCtx, w.cfg.BatchDelay); err != nil {
				return ctx.Err()
			}
		}
		end := start + min(w.cfg.MaxConcurrentChecks, len(assignment.Targets)-start)
		err := w.checkBatch(jobCtx, assignment.RoundID, assignment.Targets[start:end])
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errRoundClosed) || jobCtx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		start = end
	}
	return nil
}

// checkBatch uploads observations as they complete, so a slow target cannot
// hold finished results past the round deadline. Results that complete while
// an upload is in flight are coalesced into the next report. Every check exits
// before return, preserving the concurrency bound across batches.
func (w *Worker) checkBatch(ctx context.Context, roundID int64, targets []protocol.Target) error {
	ctx, cancel := context.WithCancel(ctx)
	completed := make(chan protocol.Result, len(targets))
	for _, target := range targets {
		go func() {
			// Cancellation yields no observation, not a failed connection.
			result, _ := w.checker.Check(ctx, target)
			completed <- result
		}()
	}
	remaining := len(targets)
	defer func() {
		cancel()
		for ; remaining > 0; remaining-- {
			<-completed
		}
	}()
	pending := make([]protocol.Result, 0, len(targets))
	for remaining > 0 {
		pending = appendObservation(pending[:0], <-completed)
		remaining--
		for remaining > 0 && len(completed) > 0 {
			pending = appendObservation(pending, <-completed)
			remaining--
		}
		if len(pending) == 0 {
			continue
		}
		// submit marshals the report before returning, so pending is reusable.
		if err := w.submit(ctx, protocol.ReportRequest{RoundID: roundID, Results: pending}); err != nil {
			return err
		}
	}
	return nil
}

func appendObservation(results []protocol.Result, result protocol.Result) []protocol.Result {
	if result.Status == protocol.Up || result.Status == protocol.Down {
		return append(results, result)
	}
	return results
}

func (w *Worker) submit(ctx context.Context, report protocol.ReportRequest) error {
	// Marshal once: an ambiguous acknowledgement is retried with the exact
	// observations, never by checking again and inventing a different status.
	body, err := json.Marshal(report)
	if err != nil {
		return err
	}
	for {
		var response protocol.ReportResponse
		err := w.postJSON(ctx, protocol.ResultsPath, body, &response)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.status == http.StatusGone {
			return errRoundClosed
		}
		if permanent(err) {
			return err
		}
		log.Printf("probe result upload: %v", err)
		if err := pause(ctx, retryDelay); err != nil {
			return err
		}
	}
}

func (w *Worker) post(ctx context.Context, path string, request, response any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return w.postJSON(ctx, path, body, response)
}

func (w *Worker) postJSON(ctx context.Context, path string, body []byte, response any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.ServerURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+w.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{status: resp.StatusCode}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(response); err != nil {
		return fmt.Errorf("decode probe API response: %w", err)
	}
	return nil
}

func pause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
