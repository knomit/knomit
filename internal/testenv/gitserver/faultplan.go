package gitserver

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

type ReqClass int

const (
	ClassOther ReqClass = iota
	ClassInfoRefs
	ClassUploadPack
	ClassReceivePack
)

// classify maps a smart-HTTP request to its git operation class.
func classify(r *http.Request) ReqClass {
	p := r.URL.Path
	switch {
	case strings.HasSuffix(p, "/info/refs"):
		switch r.URL.Query().Get("service") {
		case "git-upload-pack":
			return ClassInfoRefs // fetch advertisement
		case "git-receive-pack":
			return ClassInfoRefs // push advertisement
		}
		return ClassInfoRefs
	case strings.HasSuffix(p, "/git-upload-pack"):
		return ClassUploadPack
	case strings.HasSuffix(p, "/git-receive-pack"):
		return ClassReceivePack
	}
	return ClassOther
}

type throttleCfg struct {
	perWrite int
	delay    time.Duration
}

// FaultPlan is the mutable, mutex-guarded fault configuration for a Server.
type FaultPlan struct {
	mu       sync.Mutex
	status   map[ReqClass]int
	hang     map[ReqClass]bool
	truncate map[ReqClass]int
	throttle map[ReqClass]throttleCfg

	// auth fields
	authOn      bool
	authUser    string
	authPass    string
	expireAfter int
	authedCount int
}

func newFaultPlan() *FaultPlan {
	return &FaultPlan{
		status:   map[ReqClass]int{},
		hang:     map[ReqClass]bool{},
		truncate: map[ReqClass]int{},
		throttle: map[ReqClass]throttleCfg{},
	}
}

// SetStatus injects HTTP status code for the given request class (0 clears).
func (p *FaultPlan) SetStatus(class ReqClass, code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if code == 0 {
		delete(p.status, class)
		return
	}
	p.status[class] = code
}

// statusFor returns the injected status for a class, or 0 if none.
func (p *FaultPlan) statusFor(class ReqClass) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status[class]
}

// SetHang configures whether requests of the given class should block until the
// client's context is cancelled (hang=true), modelling a peer that accepts the
// connection but never responds (fault N3).
func (p *FaultPlan) SetHang(class ReqClass, hang bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hang[class] = hang
}

// hangFor reports whether requests of the given class should hang.
func (p *FaultPlan) hangFor(class ReqClass) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hang[class]
}

// SetTruncateAfter drops the connection after writing n body bytes for the
// given request class (N5). n=0 disables truncation.
func (p *FaultPlan) SetTruncateAfter(class ReqClass, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.truncate[class] = n
}

// SetThrottle paces body writes for the given request class (N4): each write
// is split into chunks of bytesPerWrite bytes with delay between them.
// bytesPerWrite=0 disables throttling.
func (p *FaultPlan) SetThrottle(class ReqClass, bytesPerWrite int, delay time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.throttle[class] = throttleCfg{perWrite: bytesPerWrite, delay: delay}
}

// bodyFaults returns the truncate limit and throttle config for a class.
func (p *FaultPlan) bodyFaults(class ReqClass) (truncate int, th throttleCfg) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.truncate[class], p.throttle[class]
}

// RequireBasicAuth makes every request require matching HTTP Basic credentials.
func (p *FaultPlan) RequireBasicAuth(user, pass string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authOn, p.authUser, p.authPass = true, user, pass
}

// ExpireAfter causes all requests to receive 401 after n allowed requests have
// been served (models a token going invalid). 0 disables.
//
// IMPORTANT: n counts HTTP requests, NOT git operations. A single clone or
// fetch issues ~2 requests (GET info/refs advertisement + POST upload-pack),
// and a push likewise (GET info/refs + POST receive-pack). So ExpireAfter(1)
// fails the SECOND request of the FIRST operation — useful to model expiry
// *mid-operation*, but to model "the token was valid for one full clone, then
// expired" you must pass n >= the request count of that operation (e.g. 2).
// Mid-operation 401 and between-operation expiry are distinct faults; choose n
// deliberately.
func (p *FaultPlan) ExpireAfter(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireAfter = n
}

// checkAuth returns the HTTP status to send (0 = allow). Must be called with
// the extracted BasicAuth values from the request. Increments authedCount on
// each allowed request.
func (p *FaultPlan) checkAuth(user, pass string, hasAuth bool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.authOn {
		if !hasAuth || user != p.authUser || pass != p.authPass {
			return http.StatusUnauthorized
		}
	}
	if p.expireAfter > 0 && p.authedCount >= p.expireAfter {
		return http.StatusUnauthorized
	}
	p.authedCount++
	return 0
}
