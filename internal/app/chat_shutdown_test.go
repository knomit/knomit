package app

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knomit/internal/foundation/config"
	"knomit/test/testenv"
)

// TestApp_CloseEndsALiveChatBeforeControlDBCloses is the production ordering
// the chat package's own shutdown test cannot see (it keeps the store open):
// App.Close ends the lifetime, the chat teardown runs on its own goroutine
// and must finish its writes (the client_sessions row's ended_at, the
// chat_end log line, the cancelled turn) BEFORE manager.Close closes
// control.db.
//
// The chat is mid-turn: the fake Anthropic endpoint holds the model request
// open, so Close has a running turn to cancel, not an idle chat. After Close
// the log's LAST line must be chat_end: a turn still writing would append
// after it.
//
// SABOTAGE: drop the WaitReleased call from App.Close -> manager.Close wins
// the race and the row keeps ended_at NULL / the log has no chat_end.
func TestApp_CloseEndsALiveChatBeforeControlDBCloses(t *testing.T) {
	inMessages := make(chan struct{}, 1)
	release := make(chan struct{})
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"m-one","type":"model","display_name":"M","created_at":"2026-01-01T00:00:00Z"}],"has_more":false}`)
			return
		}
		inMessages <- struct{}{}
		<-release
	}))
	defer fake.Close()
	defer close(release) // frees the held model request once the test is over

	cfg := config.Defaults()
	cfg.Home = t.TempDir()
	cfg.LLM.Providers = map[string]config.LLMProviderConfig{
		"anth": {Kind: "anthropic", BaseURL: fake.URL, APIKey: "sk-test-9f8e7d6c", Models: []string{"m-one"}, ContextWindow: 200000},
	}
	a, err := New(context.Background(), cfg, Options{APIOnly: true, Embedder: &testenv.DeterministicEmbedder{}})
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			a.Close()
		}
	}()

	call := func(method, path, body string) (int, []byte) {
		req := httptest.NewRequest(method, "/api/v1"+path, bytes.NewBufferString(body))
		req.RemoteAddr = "127.0.0.1:1"
		req.Host = "localhost"
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		a.Handler().ServeHTTP(rr, req)
		return rr.Code, rr.Body.Bytes()
	}
	code, body := call(http.MethodPost, "/chats", `{"provider":"anth","model":"m-one"}`)
	if code != http.StatusCreated {
		t.Fatalf("create chat: %d %s", code, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		t.Fatalf("create chat body: %v %s", err, body)
	}
	if code, body = call(http.MethodPost, "/chats/"+created.ID+"/turns", `{"text":"hello"}`); code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("start turn: %d %s", code, body)
	}
	select {
	case <-inMessages:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never reached the model")
	}

	a.Close()
	closed = true

	db, err := sql.Open("sqlite3", "file:"+filepath.Join(cfg.Home, "control.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ended sql.NullInt64
	if err := db.QueryRow(`SELECT ended_at FROM client_sessions WHERE id = ?`, created.ID).Scan(&ended); err != nil {
		t.Fatalf("read the chat's session row: %v", err)
	}
	if !ended.Valid {
		t.Fatal("the chat's client_sessions row has no ended_at after Close: the teardown lost the race with manager.Close")
	}

	logBytes, err := os.ReadFile(filepath.Join(cfg.Home, "chat-logs", created.ID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(logBytes)), "\n")
	var last struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last.Type != "chat_end" {
		t.Fatalf("the session log does not end with chat_end: %q", lines[len(lines)-1])
	}
}
