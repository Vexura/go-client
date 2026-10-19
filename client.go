// Package vexura provides a server-side JSON client for the Vexura prepaid API.
// Persist mutation keys and payloads before requests; the client never retries them.
package vexura

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.vexura.eu/api/v1"
const maxResponse = 8 * 1024 * 1024

// Options allows an alternate HTTPS endpoint and a transport for testing or proxies.
type Options struct {
	BaseURL   string
	Timeout   time.Duration
	Transport http.RoundTripper
}

type Client struct {
	token   string
	baseURL string
	http    *http.Client
}

// APIError excludes response bodies and credentials. Status 0 is an unconfirmed result.
type APIError struct {
	Status     int
	Code       string
	RequestID  string
	RetryAfter int
}

func (e *APIError) Error() string { return fmt.Sprintf("Vexura API: HTTP %d (%s)", e.Status, e.Code) }

// String prevents accidental credential disclosure in routine formatting.
func (c *Client) String() string   { return "Vexura Client (credentials redacted)" }
func (c *Client) GoString() string { return c.String() }

func New(token string, options *Options) (*Client, error) {
	o := Options{BaseURL: DefaultBaseURL, Timeout: 45 * time.Second}
	if options != nil {
		if options.BaseURL != "" {
			o.BaseURL = options.BaseURL
		}
		if options.Timeout != 0 {
			o.Timeout = options.Timeout
		}
		o.Transport = options.Transport
	}
	u, err := url.Parse(o.BaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimRight(u.Path, "/") != "/api/v1" {
		return nil, fmt.Errorf("use an HTTPS API URL ending in /api/v1")
	}
	if token == "" || regexp.MustCompile(`[\x00-\x20\x7f]`).MatchString(token) || o.Timeout <= 0 {
		return nil, fmt.Errorf("invalid credentials or timeout")
	}
	return &Client{token: token, baseURL: strings.TrimRight(o.BaseURL, "/"), http: &http.Client{
		Timeout: o.Timeout, Transport: o.Transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Request preserves JSON envelopes, including product-specific response shapes.
// For mutations, key must be the persisted 8-128 character Idempotency-Key.
func (c *Client) Request(ctx context.Context, method, path string, query url.Values, body any, key string, result any) error {
	if !regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE)$`).MatchString(method) {
		return fmt.Errorf("invalid method")
	}
	decoded, err := url.PathUnescape(path)
	if err != nil || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || regexp.MustCompile(`[{}?#\\\x00-\x20]`).MatchString(path) || regexp.MustCompile(`(^|/)\.{1,2}(/|$)|[?#\\\x00-\x20]`).MatchString(decoded) {
		return fmt.Errorf("invalid API path")
	}
	if (method != "GET" && key == "") || (key != "" && !regexp.MustCompile(`^[a-zA-Z0-9._:-]{8,128}$`).MatchString(key)) {
		return fmt.Errorf("persist a valid Idempotency-Key before mutations")
	}
	if method == "GET" && body != nil {
		return fmt.Errorf("GET requests cannot have a body")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("request body cannot be encoded as JSON")
		}
		reader = bytes.NewReader(encoded)
	}
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("invalid request")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Vexura-Go/1.0")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return &APIError{Code: "transport_failed"}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil {
		return &APIError{Code: "transport_failed"}
	}
	if len(raw) > maxResponse {
		return &APIError{Code: "response_too_large"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var payload struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &payload)
		if !regexp.MustCompile(`^[a-z_]{1,64}$`).MatchString(payload.Code) {
			payload.Code = ""
		}
		requestID := response.Header.Get("X-Request-ID")
		if !regexp.MustCompile(`^[a-zA-Z0-9-]{1,64}$`).MatchString(requestID) {
			requestID = ""
		}
		retry, _ := strconv.Atoi(response.Header.Get("Retry-After"))
		if retry < 0 {
			retry = 0
		}
		return &APIError{Status: response.StatusCode, Code: payload.Code, RequestID: requestID, RetryAfter: retry}
	}
	if response.StatusCode == 204 {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') || !json.Valid(trimmed) {
		return &APIError{Status: 502, Code: "invalid_response"}
	}
	if result == nil {
		return nil
	}
	if json.Unmarshal(raw, result) != nil {
		return &APIError{Status: 502, Code: "invalid_response"}
	}
	return nil
}

type Envelope[T any] struct {
	Data T `json:"data"`
}
type Team struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type Account struct {
	Team     Team     `json:"team"`
	Balance  string   `json:"balance"`
	Currency string   `json:"currency"`
	Scopes   []string `json:"scopes"`
}
type Product struct {
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Orderable  bool   `json:"orderable"`
	Operations int    `json:"operations"`
}
type RenewalQuote struct {
	Total     json.Number `json:"total"`
	ExpiresAt string      `json:"expires_at"`
}

// RenewalPayload is for monthly subscriptions. Domains and SSL use dedicated endpoints.
type RenewalPayload struct {
	Months            int         `json:"months"`
	ExpectedAmount    json.Number `json:"expected_amount"`
	ExpectedExpiresAt string      `json:"expected_expires_at"`
}

func (c *Client) Account(ctx context.Context) (Envelope[Account], error) {
	var result Envelope[Account]
	err := c.Request(ctx, "GET", "/account", nil, nil, "", &result)
	return result, err
}

func (c *Client) Products(ctx context.Context) (Envelope[[]Product], error) {
	var result Envelope[[]Product]
	err := c.Request(ctx, "GET", "/products", nil, nil, "", &result)
	return result, err
}

func (c *Client) RenewalQuote(ctx context.Context, subscription string, period int, unit string) (Envelope[RenewalQuote], error) {
	var result Envelope[RenewalQuote]
	if unit != "months" && unit != "years" {
		return result, fmt.Errorf("unit must be months or years")
	}
	err := c.Request(ctx, "GET", "/subscriptions/"+url.PathEscape(subscription)+"/quote", url.Values{unit: {strconv.Itoa(period)}}, nil, "", &result)
	return result, err
}

func (c *Client) Renew(ctx context.Context, subscription string, payload RenewalPayload, key string, result any) error {
	return c.Request(ctx, "POST", "/subscriptions/"+url.PathEscape(subscription)+"/renew", nil, payload, key, result)
}
