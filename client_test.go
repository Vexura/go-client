package vexura

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAccountAndQuote(t *testing.T) {
	client, err := New("example-test-token", &Options{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer example-test-token" || r.Header.Get("User-Agent") == "" {
			t.Fatal("Missing auth or UA")
		}
		if r.URL.Path != "/api/v1/account" && r.URL.RawQuery != "years=2" {
			t.Fatal(r.URL.String())
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"data":{"balance":"10.00","total":24}}`))}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	account, err := client.Account(context.Background())
	if err != nil || account.Data.Balance != "10.00" {
		t.Fatal(account, err)
	}
	quote, err := client.RenewalQuote(context.Background(), "example", 2, "years")
	if err != nil || quote.Data.Total != "24" {
		t.Fatal(quote, err)
	}
	if strings.Contains(fmt.Sprintf("%#v", client), "example-test-token") {
		t.Fatal("Leaked credentials")
	}
}

func TestMutationAndNoRetry(t *testing.T) {
	calls := 0
	client, _ := New("test", &Options{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Idempotency-Key") != "invoice-12345" {
			t.Fatal("Missing persisted key")
		}
		return nil, fmt.Errorf("secret upstream failure")
	})})
	if client.Request(context.Background(), "POST", "/order", nil, nil, "", nil) == nil {
		t.Fatal("Accepted missing key")
	}
	err := client.Renew(context.Background(), "example", RenewalPayload{Months: 1, ExpectedAmount: "4", ExpectedExpiresAt: "2027-01-01"}, "invoice-12345", nil)
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.Status != 0 || calls != 1 || strings.Contains(err.Error(), "secret") {
		t.Fatal(err, calls)
	}
}

func TestPathsAndErrors(t *testing.T) {
	client, _ := New("test", nil)
	for _, path := range []string{"//evil.test", "/../account", "/%2e%2e/account", "/a%3Fb", "/a\\b"} {
		if client.Request(context.Background(), "GET", path, nil, nil, "", nil) == nil {
			t.Fatal("Accepted path", path)
		}
	}
	for _, url := range []string{"http://api.vexura.eu/api/v1", "https://secret@api.vexura.eu/api/v1", "https://api.vexura.eu/api/v1?key=x"} {
		if _, err := New("test", &Options{BaseURL: url}); err == nil {
			t.Fatal("Accepted URL")
		}
	}
	for _, status := range []int{204, 302, 429, 500} {
		client, _ := New("test", &Options{Transport: transportFunc(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"60"}, "X-Request-Id": {"request-123"}}, Body: io.NopCloser(strings.NewReader(`{"code":"rate_limited","message":"secret"}`))}, nil
		})})
		err := client.Request(context.Background(), "GET", "/account", nil, nil, "", nil)
		if status == 204 {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		var apiError *APIError
		if !errors.As(err, &apiError) || apiError.Status != status || apiError.RetryAfter != 60 || strings.Contains(err.Error(), "secret") {
			t.Fatal(err)
		}
	}
}
