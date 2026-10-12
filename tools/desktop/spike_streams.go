//go:build desktop

package main

// SPIKE (spike/wails-asset-transport, spike 2): the desktop holds each SSE
// subscription itself, over the local socket/pipe, and forwards frames to the
// page through one Wails Stream. No listener exists. Behind
// KNOMIT_SPIKE_PROXY=1 && KNOMIT_SPIKE_STREAMS=1.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	"knomit/internal/auth"
	"knomit/internal/config"
)

const spikeStreamName = "knomit-sse"

var (
	spikeStreamConnsMu sync.Mutex
	spikeStreamConns   = map[*spikeConn]struct{}{}
)

// spikeDropStreamUpstreams closes every stream upstream connection from the
// desktop side, to exercise reconnect.
func spikeDropStreamUpstreams() int {
	spikeStreamConnsMu.Lock()
	defer spikeStreamConnsMu.Unlock()
	n := 0
	for c := range spikeStreamConns {
		_ = c.Conn.Close()
		n++
	}
	return n
}

func spikeStreamsEnabled() bool {
	return spikeEnabled() && os.Getenv("KNOMIT_SPIKE_STREAMS") == "1"
}

// spikeFrame is one Go->page frame; see DesktopFrame in web/src/eventStream.ts.
type spikeFrame struct {
	T     string `json:"t"`
	Event string `json:"event,omitempty"`
	Data  string `json:"data,omitempty"`
	ID    string `json:"id,omitempty"`
}

type spikeSubscribe struct {
	Path        string `json:"path"`
	LastEventID string `json:"last_event_id,omitempty"`
}

func spikeRegisterStreams(wapp *application.App) {
	if !spikeStreamsEnabled() {
		return
	}
	sock, err := config.SocketPath()
	if err != nil {
		spikeLogf("streams: no socket path: %v", err)
		return
	}
	// One connection per subscription (no keep-alive pooling) so that
	// spikeOpenConns counts subscriptions exactly and a close is visible.
	tr := &http.Transport{
		DisableCompression: true,
		DisableKeepAlives:  true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, err := auth.DialLocal(ctx, sock, 5*time.Second)
			if err != nil {
				return nil, err
			}
			spikeOpenConns.Add(1)
			spikeTotalConns.Add(1)
			spikeLogf("stream upstream conn opened open=%d", spikeOpenConns.Load())
			sc := &spikeConn{Conn: c}
			spikeStreamConnsMu.Lock()
			spikeStreamConns[sc] = struct{}{}
			spikeStreamConnsMu.Unlock()
			return sc, nil
		},
	}
	wapp.HandleStream(spikeStreamName, func(c *application.StreamConn) {
		var sub spikeSubscribe
		if err := c.ReceiveJSON(&sub); err != nil {
			return
		}
		// Only API reads: the page names a path, never a host or another verb.
		if !strings.HasPrefix(sub.Path, "/api/v1/") || strings.Contains(sub.Path, "..") {
			spikeLogf("stream refused path %q", sub.Path)
			return
		}
		spikeLogf("stream open %s", sub.Path)
		defer func() { spikeLogf("stream handler end %s ctxErr=%v", sub.Path, c.Context().Err()) }()
		spikeRunSubscription(c, tr, sub)
	})
}

func spikeSend(c *application.StreamConn, f spikeFrame) error {
	b, _ := json.Marshal(f)
	return c.Send(b)
}

func spikeRunSubscription(c *application.StreamConn, tr *http.Transport, sub spikeSubscribe) {
	ctx := c.Context()
	lastID := sub.LastEventID
	backoff := 500 * time.Millisecond
	client := &http.Client{Transport: tr}
	for ctx.Err() == nil {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+sub.Path, nil)
		req.Header.Set("Accept", "text/event-stream")
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			backoff = 500 * time.Millisecond
			if spikeSend(c, spikeFrame{T: "open"}) != nil {
				resp.Body.Close()
				return
			}
			err = spikeReadSSE(c, resp.Body, &lastID, &backoff)
			resp.Body.Close()
		} else if err == nil {
			spikeLogf("stream upstream status %d for %s", resp.StatusCode, sub.Path)
			resp.Body.Close()
			if resp.StatusCode >= 400 && resp.StatusCode < 500 {
				_ = spikeSend(c, spikeFrame{T: "error"})
				_ = c.Close()
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		spikeLogf("stream upstream dropped (%v) retry in %s lastID=%q", err, backoff, lastID)
		if spikeSend(c, spikeFrame{T: "error"}) != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// spikeReadSSE parses text/event-stream (event:, data: multi-line, id:, retry:,
// comments) and forwards each dispatched event. Returns when the body ends.
func spikeReadSSE(c *application.StreamConn, body io.Reader, lastID *string, backoff *time.Duration) error {
	br := bufio.NewReaderSize(body, 64<<10)
	var ev, id string
	var data []string
	hasData := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if hasData {
				if id != "" {
					*lastID = id
				}
				spikeLogf("stream recv upstream event=%q bytes=%d", ev, len(strings.Join(data, "\n")))
				if err := spikeSend(c, spikeFrame{T: "event", Event: ev, Data: strings.Join(data, "\n"), ID: *lastID}); err != nil {
					return err
				}
				spikeLogf("stream sent to page event=%q", ev)
			}
			ev, data, hasData = "", nil, false
		case strings.HasPrefix(line, ":"):
		default:
			name, val, _ := strings.Cut(line, ":")
			val = strings.TrimPrefix(val, " ")
			switch name {
			case "event":
				ev = val
			case "data":
				data = append(data, val)
				hasData = true
			case "id":
				if !strings.Contains(val, "\x00") {
					id = val
				}
			case "retry":
				if ms, err := strconv.Atoi(val); err == nil && ms >= 0 {
					*backoff = time.Duration(ms) * time.Millisecond
				}
			}
		}
	}
}

// spikeInjectTransport appends the transport flag to /config.js.
func spikeInjectTransport(next http.Handler) http.Handler {
	if !spikeStreamsEnabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config.js" {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		_, _ = w.Write([]byte("window.__KNOMIT_EVENT_TRANSPORT__ = \"wails-stream\";\n"))
		if os.Getenv("KNOMIT_SPIKE_PROBE") == "1" {
			_, _ = w.Write([]byte(spikeUIProbe))
		}
	})
}

// spikeUIProbe runs inside the REAL UI page (appended to /config.js): it creates
// a repo through the UI's own origin, forces a stream resume (the server's
// `ready` frame makes the UI re-read the repo list), and logs when the rail text
// shows the repo, so the "rail updates" claim is read from the DOM.
const spikeUIProbe = `
(function(){
  var log=function(m){ try{ fetch('/spike/log',{method:'POST',body:(performance.timeOrigin+performance.now()).toFixed(1)+' UI '+m}); }catch(e){} };
  var sleep=function(ms){ return fetch('/spike/sleep?ms='+ms).then(function(r){return r.text();}); };
  window.addEventListener('load', function(){ (async function(){
    for(;;){ try{ var b=await (await fetch('/boot/status')).json(); if(b.ready) break; }catch(e){} await sleep(300); }
    await sleep(1500);
    log('probe: rail has railtest2 before? '+document.body.innerText.includes('railtest2'));
    var r=await fetch('/api/v1/repos',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({name:'railtest2',mode:'preset',ontology_preset:'default'})});
    log('probe: created '+r.status);
    await sleep(700);
    log('probe: rail has railtest2 after create, before resume? '+document.body.innerText.includes('railtest2'));
    await fetch('/spike/drop'); log('probe: dropped upstreams');
    for(var i=0;i<20;i++){ await sleep(200); if(document.body.innerText.includes('railtest2')){ log('probe: rail shows railtest2 after '+((i+1)*200)+'ms of resume'); return; } }
    log('probe: rail never showed railtest2; text='+document.body.innerText.slice(0,200).replace(/\n/g,' | '));
  })(); });
})();
`
