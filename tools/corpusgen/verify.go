package main

import (
	"context"
	"net/http"
	"time"
)

// urlVerifyTimeout bounds each HTTP check so one slow/hanging host can't
// stall an entire batch's verification.
const urlVerifyTimeout = 8 * time.Second

// verifyURL reports whether url is real and reachable: a GET that returns a
// non-error, non-4xx/5xx status. This is the one available integrity check
// on the model's self-reported citations — the model is instructed never to
// fabricate a URL, but instructions alone don't guarantee it, so every ref
// gets an actual HTTP round-trip before being trusted into the corpus.
func verifyURL(ctx context.Context, client *http.Client, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, urlVerifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; corpusgen-verify/1.0)")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 400
}

// verifyAndFilter checks every ref URL in gen (parallel to slots) and drops
// any fact with no verified citation — a "real facts" corpus shouldn't
// contain claims nobody can trace back to a real source. This includes
// facts the model honestly left uncited (the real-mode prompt explicitly
// tells it an empty refs list is fine over fabricating one — that's about
// not pressuring the model to lie, not about the corpus accepting uncited
// claims; both zero-refs and all-refs-failed-verification are dropped here
// the same way). Individually-unverifiable refs are dropped from a fact
// that has at least one good ref, rather than dropping the whole fact.
// Returns the kept (slot, content) pairs in order and how many were dropped.
func verifyAndFilter(ctx context.Context, slots []factSlot, gen []generatedContent) (keptSlots []factSlot, keptGen []generatedContent, dropped int) {
	client := &http.Client{Timeout: urlVerifyTimeout + time.Second}
	for i, g := range gen {
		var good []string
		for _, ref := range g.Refs {
			if verifyURL(ctx, client, ref) {
				good = append(good, ref)
			}
		}
		if len(good) == 0 {
			dropped++
			continue
		}
		g.Refs = good
		keptSlots = append(keptSlots, slots[i])
		keptGen = append(keptGen, g)
	}
	return keptSlots, keptGen, dropped
}
