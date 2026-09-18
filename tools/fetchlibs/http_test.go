package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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
//
// IT MUST COVER THE Retry-After PATH TOO. Computing the worst case from
// backoffFor alone would be a guarantee about the half of the wait the code
// controls: a server-supplied `Retry-After: 3600` never passes through
// backoffFor, so a bound checked there stays green while a real job sleeps an
// hour. The second half below walks the whole schedule the way httpGet does,
// with a hostile header on every attempt, and asserts the TOTAL.
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

	// The same walk httpGet performs, against a server that answers every
	// attempt with `Retry-After: 3600`.
	hostile := retryAdvice{again: true, after: time.Hour}
	var spent time.Duration
	for attempt := 1; attempt < fetchAttempts; attempt++ {
		wait := nextWait(attempt, hostile, spent)
		if wait <= 0 {
			break
		}
		spent += wait
	}
	if spent >= time.Minute {
		t.Errorf("worst case WITH Retry-After is %s; a header must not breach the "+
			"under-a-minute guarantee this test exists to make", spent)
	}
	if spent != total {
		t.Errorf("hostile-header schedule spent %s, want exactly the %s budget — "+
			"the header redistributes the waiting, it does not add to it", spent, total)
	}
}

// ---------------------------------------------------------------- 429 + Retry-After

// TestParseRetryAfter covers RFC 9110's two forms and the junk in between.
// The fallback matters as much as the parse: anything unusable must leave the
// caller on its own backoff rather than on a zero wait.
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{"delay-seconds", "5", 5 * time.Second, true},
		{"zero seconds", "0", 0, true},
		{"surrounding space", "  7 ", 7 * time.Second, true},
		{"http-date in the future", "Fri, 18 Sep 2026 12:00:30 GMT", 30 * time.Second, true},
		{"http-date in the past", "Fri, 18 Sep 2026 11:59:00 GMT", 0, true},
		{"negative seconds", "-5", 0, false},
		{"not a number or a date", "soon", 0, false},
		{"absent", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseRetryAfter(tc.value, now)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("duration = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestNextWait pins the three rules the wait obeys, without sleeping: the
// doubling backoff is the floor, a longer Retry-After raises it, and the
// overall budget is the ceiling that a hostile or absent-minded header cannot
// breach. A non-positive result means "no budget left, stop retrying".
func TestNextWait(t *testing.T) {
	prev := fetchRetryDelay
	fetchRetryDelay = 2 * time.Second
	t.Cleanup(func() { fetchRetryDelay = prev })

	budget := retryBudget() // 2 + 4 + 8 = 14s at the defaults

	t.Run("no advice uses the doubling backoff", func(t *testing.T) {
		if got := nextWait(1, retryAdvice{again: true}, 0); got != 2*time.Second {
			t.Errorf("got %s, want 2s", got)
		}
		if got := nextWait(3, retryAdvice{again: true}, 0); got != 8*time.Second {
			t.Errorf("got %s, want 8s", got)
		}
	})

	t.Run("a longer Retry-After wins", func(t *testing.T) {
		got := nextWait(1, retryAdvice{again: true, after: 5 * time.Second}, 0)
		if got != 5*time.Second {
			t.Errorf("got %s, want the header's 5s, not the 2s backoff", got)
		}
	})

	t.Run("a shorter Retry-After does not shorten the backoff", func(t *testing.T) {
		got := nextWait(2, retryAdvice{again: true, after: time.Second}, 0)
		if got != 4*time.Second {
			t.Errorf("got %s, want the 4s backoff — a server may ask us to wait "+
				"longer, never to hammer it sooner", got)
		}
	})

	t.Run("an absurd Retry-After is clamped to the budget", func(t *testing.T) {
		got := nextWait(1, retryAdvice{again: true, after: time.Hour}, 0)
		if got != budget {
			t.Errorf("got %s, want the whole remaining budget %s — a "+
				"Retry-After: 3600 must not turn a CI step into an hour-long hang",
				got, budget)
		}
	})

	t.Run("spent budget is subtracted", func(t *testing.T) {
		got := nextWait(3, retryAdvice{again: true, after: time.Hour}, 12*time.Second)
		if got != 2*time.Second {
			t.Errorf("got %s, want the 2s left of the %s budget", got, budget)
		}
	})

	t.Run("an exhausted budget stops the retrying", func(t *testing.T) {
		if got := nextWait(3, retryAdvice{again: true, after: time.Hour}, budget); got > 0 {
			t.Errorf("got %s, want a non-positive wait meaning 'give up'", got)
		}
	})
}

// TestHTTPGet_RetriesRateLimitHonouringRetryAfter: 429 is the one 4xx that IS
// retried. A rate limit is transient by definition — it is the class this
// retry exists for — while every other 4xx says the pin in spec.go is wrong.
//
// The claim under test is only observable in real elapsed time, so this one
// actually sleeps. The backoff is shrunk to 200ms — far BELOW the 1s the
// header asks for, and with a 1.4s budget that does not clamp it — so an
// elapsed second can only have come from the header. Ignoring Retry-After
// would finish in 200ms and fail here.
func TestHTTPGet_RetriesRateLimitHonouringRetryAfter(t *testing.T) {
	var log bytes.Buffer
	prevDelay, prevLog := fetchRetryDelay, retryLog
	fetchRetryDelay, retryLog = 200*time.Millisecond, &log
	t.Cleanup(func() { fetchRetryDelay, retryLog = prevDelay, prevLog })

	const retryAfterSecs = 1

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSecs))
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, "the-real-archive")
	}))
	defer srv.Close()

	start := time.Now()
	body, err := httpGet(srv.URL)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("expected the second attempt to succeed, got %v", err)
	}
	defer body.Close()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the-real-archive" {
		t.Errorf("body = %q, want the payload served after the rate limit lifted", got)
	}
	if n := requests.Load(); n != 2 {
		t.Errorf("server saw %d requests, want exactly 2 (the 429 then the success)", n)
	}
	// The header asked for 1s and the default backoff for attempt 1 is 2s, so
	// the floor asserted here is the HEADER's value: it proves the header was
	// read and not ignored only in combination with the clamp tests above,
	// which is why both exist.
	if want := retryAfterSecs * time.Second; elapsed < want {
		t.Errorf("waited %s before retrying, want at least the Retry-After of %s", elapsed, want)
	}
	if !strings.Contains(log.String(), "retrying") {
		t.Errorf("the rate-limit retry was not announced; log was:\n%s", log.String())
	}
}

// TestHTTPGet_DoesNotRetryOtherClientErrors is the boundary case for the one
// exception above: 429 is retried, 403 — the other 4xx a CDN actually emits —
// is not.
func TestHTTPGet_DoesNotRetryOtherClientErrors(t *testing.T) {
	shortRetries(t)

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()

	body, err := httpGet(srv.URL)
	if err == nil {
		body.Close()
		t.Fatal("expected a 403 to be an error")
	}
	if n := requests.Load(); n != 1 {
		t.Errorf("server saw %d requests, want exactly 1 — 429 is the ONLY retried 4xx", n)
	}
}
