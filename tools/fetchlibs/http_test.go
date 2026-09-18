package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The retry tests count requests ON THE SERVER SIDE. Counting attempts inside
// httpGet would only prove the loop ran the number of times the loop was told
// to run; counting arrivals proves the requests were actually made, which is
// the claim ("500, 500, 200 succeeds after three requests") that the CI
// failure this fixes was about.
//
// They also pin the delay to something unobservable and capture the retry log,
// so a run costs microseconds and the "a CI log shows it happened" requirement
// is asserted rather than assumed.

// shortRetries makes the backoff invisible and redirects the retry log into a
// buffer for the duration of one test.
func shortRetries(t *testing.T) *bytes.Buffer {
	t.Helper()
	var log bytes.Buffer
	prevDelay, prevLog := fetchRetryDelay, retryLog
	fetchRetryDelay, retryLog = time.Millisecond, &log
	t.Cleanup(func() { fetchRetryDelay, retryLog = prevDelay, prevLog })
	return &log
}

// TestHTTPGet_RetriesServerErrorsThenSucceeds is the regression test for job
// 105443136472: a single 500 from the GitHub release CDN failed `make setup`
// and with it the whole build-test job.
func TestHTTPGet_RetriesServerErrorsThenSucceeds(t *testing.T) {
	log := shortRetries(t)

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) <= 2 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, "the-real-archive")
	}))
	defer srv.Close()

	body, err := httpGet(srv.URL)
	if err != nil {
		t.Fatalf("expected the third attempt to succeed, got %v", err)
	}
	defer body.Close()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	// WHICH body came back, not merely that one did: returning the 500's
	// "boom" page would be the exact failure writeFile's atomic rename cannot
	// catch, because an error page is a perfectly well-formed file.
	if string(got) != "the-real-archive" {
		t.Errorf("body = %q, want the payload served by the successful attempt", got)
	}
	if n := requests.Load(); n != 3 {
		t.Errorf("server saw %d requests, want exactly 3 (two 500s then the success)", n)
	}
	// Two retries, therefore two lines. A silent retry is a build that looks
	// like it hung for 6 seconds for no reason.
	if n := strings.Count(log.String(), "retrying"); n != 2 {
		t.Errorf("retry log has %d 'retrying' lines, want 2; log was:\n%s", n, log.String())
	}
}

// TestHTTPGet_DoesNotRetryNotFound: a 404 is a WRONG PIN in
// tools/fetchlibs/spec.go, not a transient failure. Retrying it turns a
// 1-second "you edited ortVersion to something that does not exist" into a
// 15-second one and tells the reader nothing new.
func TestHTTPGet_DoesNotRetryNotFound(t *testing.T) {
	shortRetries(t)

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	body, err := httpGet(srv.URL)
	if err == nil {
		body.Close()
		t.Fatal("expected a 404 to be an error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error %q should name the status", err)
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("server saw %d requests, want exactly 1 — a 4xx must not be retried", n)
	}
}

// TestHTTPGet_GivesUpAfterBoundedAttempts: the retry is BOUNDED. An
// unbounded one would turn a release outage from a fast red build into a job
// that burns its timeout.
func TestHTTPGet_GivesUpAfterBoundedAttempts(t *testing.T) {
	shortRetries(t)

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "still down", http.StatusBadGateway)
	}))
	defer srv.Close()

	body, err := httpGet(srv.URL)
	if err == nil {
		body.Close()
		t.Fatal("expected an all-5xx server to fail")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error %q should carry the last status", err)
	}
	if n := requests.Load(); int(n) != fetchAttempts {
		t.Errorf("server saw %d requests, want exactly fetchAttempts=%d", n, fetchAttempts)
	}
}

// TestHTTPGet_RetriesTransportErrors: the other half of the transient class.
// The handler hijacks and closes the connection without writing a response, so
// the client sees an EOF from the transport rather than a status — and the
// count still comes from the server, which observed every arrival before
// hanging up.
func TestHTTPGet_RetriesTransportErrors(t *testing.T) {
	shortRetries(t)

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_ = conn.(*net.TCPConn).SetLinger(0)
		_ = conn.Close()
	}))
	defer srv.Close()

	body, err := httpGet(srv.URL)
	if err == nil {
		body.Close()
		t.Fatal("expected a dropped connection to fail")
	}
	if n := requests.Load(); int(n) != fetchAttempts {
		t.Errorf("server saw %d requests, want exactly fetchAttempts=%d", n, fetchAttempts)
	}
}

// TestFetchRetryPolicy_IsBoundedAndShortEnoughForCI keeps the two constants
// honest against the rule they were chosen under: a handful of attempts, and a
// total wait a person watching a CI job would not mistake for a hang.
func TestFetchRetryPolicy_IsBoundedAndShortEnoughForCI(t *testing.T) {
	if fetchAttempts < 2 || fetchAttempts > 5 {
		t.Errorf("fetchAttempts = %d, want a small bounded count (2..5)", fetchAttempts)
	}
	var total time.Duration
	for attempt := 1; attempt < fetchAttempts; attempt++ {
		total += backoffFor(attempt)
	}
	if total >= time.Minute {
		t.Errorf("worst-case backoff %s must stay under a minute", total)
	}
	if total < time.Second {
		t.Errorf("worst-case backoff %s is too short to outlast a CDN blip", total)
	}
}
