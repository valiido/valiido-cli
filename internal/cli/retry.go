package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxRequestRetries = 3

var retryBackoffBase = 500 * time.Millisecond

type retryKey struct{}

type retryPolicy struct {
	transient             bool
	rateLimited           bool
	keepRateLimitOnCancel bool
}

func apiRetryPolicy(method, path string) retryPolicy {
	path, _, _ = strings.Cut(path, "?")
	switch {
	case method == http.MethodGet:
		return retryPolicy{transient: true, rateLimited: true}
	case path == apiPaths["api/commands#create"],
		path == apiPaths["api/commands#confirm"],
		path == apiPaths["api/commands#reject"],
		path == apiPaths["oauth#revoke"]:
		// Rails prepares writes, and resolve actions serialize token consumption.
		return retryPolicy{transient: true, rateLimited: true}
	case path == apiPaths["oauth#token"]:
		// Preserve a definitive rejection if the context expires during backoff.
		// An ambiguous transport failure may instead mean a token was consumed.
		return retryPolicy{rateLimited: true, keepRateLimitOnCancel: true}
	case path == apiPaths["api/uploads#create"], path == apiPaths["oauth#device_authorization"]:
		// Rate limiting precedes allocation. An ambiguous POST may allocate an upload or pairing.
		return retryPolicy{rateLimited: true}
	default:
		return retryPolicy{}
	}
}

func transientStatus(status int) bool {
	return status == 502 || status == 503 || status == 504
}

func (c *client) withRetry(ctx context.Context, policy retryPolicy, timeout time.Duration, send func() (*http.Response, error)) (*http.Response, error) {
	lastStatus := 0
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			if policy.keepRateLimitOnCancel && lastStatus == http.StatusTooManyRequests {
				return nil, responseError(lastStatus, nil)
			}
			return nil, failure("cancelled", "Request cancelled or pairing expired.")
		}
		resp, err := send()
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		if err != nil {
			var typed *apiError
			if errors.As(err, &typed) {
				return nil, err
			}
		}
		retry := (policy.transient && (err != nil && retryableTransport(err) || transientStatus(status))) ||
			(policy.rateLimited && status == 429)
		if !retry || attempt == maxRequestRetries {
			if err != nil {
				return nil, transportError(ctx, err, timeout)
			}
			return resp, nil
		}
		delay := retryAfterDelay(resp, attempt)
		if deadline, ok := ctx.Deadline(); ok && resp != nil && time.Now().Add(delay).After(deadline) {
			// Waiting would only end in cancellation; report the server's answer instead.
			return resp, nil
		}
		if resp != nil {
			resp.Body.Close()
		}
		if c.retryLog != nil {
			reason := "Transient request failure"
			switch {
			case status == 429:
				reason = "Rate limited"
			case status != 0:
				reason = fmt.Sprintf("HTTP %d from server", status)
			}
			fmt.Fprintf(c.retryLog, "%s; retrying in %s (%d/%d).\n", reason, delay.Round(time.Millisecond), attempt+1, maxRequestRetries)
		}
		lastStatus = status
		if !waitFor(ctx, delay) {
			continue
		}
		if count, ok := ctx.Value(retryKey{}).(*int); ok {
			(*count)++
		}
	}
}

func waitFor(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

func retryAfterDelay(resp *http.Response, attempt int) time.Duration {
	if resp != nil {
		header := strings.TrimSpace(resp.Header.Get("Retry-After"))
		if seconds, err := strconv.ParseUint(header, 10, 64); err == nil {
			const maxSeconds = uint64((1<<63 - 1) / time.Second)
			return time.Duration(min(seconds, maxSeconds)) * time.Second
		}
		if at, err := http.ParseTime(header); err == nil {
			return max(0, time.Until(at))
		}
	}
	if retryBackoffBase <= 0 {
		return 0
	}
	delay := retryBackoffBase * [...]time.Duration{1, 3, 8}[attempt]
	return delay + rand.N(delay/4+1)
}

func bufferResponse(resp *http.Response, limit int64) (*http.Response, error) {
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, failure("protocol", "API response exceeded the size limit.")
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}

func tlsFailure(err error) bool {
	var verification *tls.CertificateVerificationError
	var header tls.RecordHeaderError
	var unknownAuthority x509.UnknownAuthorityError
	var alert tls.AlertError
	return errors.As(err, &verification) || errors.As(err, &header) || errors.As(err, &unknownAuthority) ||
		errors.As(err, &alert) || strings.Contains(err.Error(), "tls:")
}

func retryableTransport(err error) bool {
	var dns *net.DNSError
	return !errors.Is(err, context.Canceled) && !tlsFailure(err) &&
		(!errors.As(err, &dns) || !dns.IsNotFound)
}

func transportError(ctx context.Context, err error, timeout time.Duration) error {
	var dns *net.DNSError
	var network net.Error
	switch {
	case ctx.Err() != nil:
		return failure("cancelled", "Request cancelled or pairing expired.")
	case errors.As(err, &dns):
		return failure("network", "DNS resolution failed.")
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout():
		return failure("network", fmt.Sprintf("Request timed out (limit %s).", timeout))
	case tlsFailure(err):
		return failure("network", "TLS connection failed.")
	case errors.Is(err, io.ErrUnexpectedEOF):
		return failure("network", "Connection closed before the response was complete.")
	default:
		return failure("network", "Could not connect to the server or the connection was interrupted.")
	}
}

func uncertainWrite(err error, hint string) error {
	var ae *apiError
	if errors.As(err, &ae) {
		switch ae.Code {
		case "not_found", "network", "server_unavailable", "protocol", "api", "cancelled":
			copy := *ae
			copy.Hint = hint
			return &copy
		}
	}
	return err
}
