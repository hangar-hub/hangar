package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Exit codes from docs/spec/08-local-tools.md.
const (
	ExitOK       = 0
	ExitFailure  = 1
	ExitUsage    = 2
	ExitNotFound = 3
	ExitAuth     = 4
)

// ExitError carries the exit code a command should end with.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode returns the process exit code for an error returned by a
// command: 0 for nil, the code of an *ExitError, and 1 otherwise.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	return ExitFailure
}

// usagef returns an error that exits with code 2.
func usagef(format string, a ...any) error {
	return &ExitError{Code: ExitUsage, Err: fmt.Errorf(format, a...)}
}

// notFoundf returns an error that exits with code 3.
func notFoundf(format string, a ...any) error {
	return &ExitError{Code: ExitNotFound, Err: fmt.Errorf(format, a...)}
}

// authf returns an error that exits with code 4.
func authf(format string, a ...any) error {
	return &ExitError{Code: ExitAuth, Err: fmt.Errorf(format, a...)}
}

// APIError is an error answer from an Instance, from the native API's
// problem details or the Hub API's X-Error-Message header.
type APIError struct {
	Status  int
	Message string
	// Retryable is true for a 503 with a Retry-After header, which the
	// native API sends when it kept the request's state and the same
	// request may be sent again after RetryAfter.
	Retryable  bool
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("the Instance answered %d %s", e.Status, http.StatusText(e.Status))
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Status)
}

// IsNotFound reports whether err is a 404 from an Instance.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// apiError builds an error from a failed response and maps its status to an
// exit code: 401 and 403 exit with 4, 404 with 3, everything else with 1.
func apiError(status int, header http.Header, body []byte) error {
	ae := &APIError{Status: status, Message: errorMessage(header, body)}
	if status == http.StatusServiceUnavailable {
		ae.RetryAfter, ae.Retryable = parseRetryAfter(header.Get("Retry-After"), time.Now())
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		if status == http.StatusUnauthorized && ae.Message == "" {
			ae.Message = "sign in with hangar login, or check your Token"
		}
		return &ExitError{Code: ExitAuth, Err: ae}
	case http.StatusNotFound:
		return &ExitError{Code: ExitNotFound, Err: ae}
	default:
		return &ExitError{Code: ExitFailure, Err: ae}
	}
}

// parseRetryAfter reads a Retry-After value, in seconds or as an HTTP date,
// as a wait from now. It reports false for a missing or malformed value.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		return time.Duration(min(secs, int64(maxRetryAfter/time.Second))) * time.Second, true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	return min(max(t.Sub(now), 0), maxRetryAfter), true
}

// maxRetryAfter bounds how long the CLI waits for one Retry-After, so an
// Instance or proxy cannot stall a command for days.
const maxRetryAfter = 10 * time.Minute

// errorMessage extracts a human-readable message from an error response.
func errorMessage(header http.Header, body []byte) string {
	if m := header.Get("X-Error-Message"); m != "" {
		return m
	}
	var p struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
		Error  string `json:"error"`
		Errors []struct {
			Detail string `json:"detail"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &p) == nil {
		msg := p.Detail
		if msg == "" {
			msg = p.Title
		}
		if msg == "" {
			msg = p.Error
		}
		for _, e := range p.Errors {
			if e.Detail != "" {
				msg += "; " + e.Detail
			}
		}
		if msg != "" {
			return msg
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 || strings.HasPrefix(s, "<") {
		return ""
	}
	return s
}
