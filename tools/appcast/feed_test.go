package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/appcast"
)

// testItems includes a linux entry even though ItemsFromReleases never
// produces one today. BuildFeed is format-generic; keeping linux covered here
// proves the XML shape is already correct for the day the upstream $APPIMAGE
// fix lands and Linux is switched on.
func testItems() []Item {
	pub := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	return []Item{
		{
			Version: "0.5.1", Title: "0.5.1", Notes: "Fixes & things",
			OS: "darwin", URL: "https://example.test/Knomit-0.5.1-darwin-arm64.app.zip",
			Length: 1234, EdSignature: "AAAA", PublishedAt: pub,
		},
		{
			Version: "0.5.1", Title: "0.5.1", Notes: "Fixes & things",
			OS: "linux", URL: "https://example.test/Knomit-0.5.1-linux-amd64.AppImage",
			Length: 5678, EdSignature: "BBBB", PublishedAt: pub,
		},
	}
}

// The exact sparkle:os spelling matters: platformMatches aliases macos/mac/osx
// to darwin but has NO alias for linux, and compares against runtime.GOOS.
// "macos" here would still work; "Linux" or "gnu-linux" would silently make
// every Linux client see no update at all.
func TestBuildFeedEmitsGOOSPlatformNames(t *testing.T) {
	out, err := BuildFeed("https://knomit.github.io/knomit/appcast.xml", testItems())
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{
		"<sparkle:os>darwin</sparkle:os>",
		"<sparkle:os>linux</sparkle:os>",
		"<sparkle:shortVersionString>0.5.1</sparkle:shortVersionString>",
		`sparkle:edSignature="AAAA"`,
		`length="1234"`,
		"Tue, 28 Jul 2026 12:00:00 +0000",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("feed missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "<sparkle:os>macos</sparkle:os>") {
		t.Error("feed used macos; the parser compares against runtime.GOOS")
	}
}

// BuildFeed must never emit an item it cannot authenticate: pkg/updater
// installs a release carrying no verification block without complaint.
func TestBuildFeedRefusesUnsignedItems(t *testing.T) {
	items := testItems()
	items[0].EdSignature = ""
	if _, err := BuildFeed("https://example.test/appcast.xml", items); err == nil {
		t.Error("BuildFeed emitted an unsigned item, want an error")
	}
}

// End-to-end through the REAL provider: serve our generated feed over HTTP and
// confirm wails' appcast provider selects the right per-OS item and surfaces
// our signature. This is the seam that would otherwise fail only in production.
func TestGeneratedFeedParsesInWailsProvider(t *testing.T) {
	feed, err := BuildFeed("https://example.test/appcast.xml", testItems())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write(feed)
	}))
	defer srv.Close()

	prov, err := appcast.New(appcast.Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ platform, wantURL string }{
		{"darwin", "https://example.test/Knomit-0.5.1-darwin-arm64.app.zip"},
		{"linux", "https://example.test/Knomit-0.5.1-linux-amd64.AppImage"},
	} {
		rel, rerr := prov.Check(t.Context(), updater.CheckRequest{
			CurrentVersion: "0.5.0", Platform: tc.platform, Arch: "amd64",
		})
		if rerr != nil {
			t.Fatalf("%s: Check: %v", tc.platform, rerr)
		}
		if rel == nil {
			t.Fatalf("%s: Check returned no release; 0.5.1 should beat 0.5.0", tc.platform)
		}
		if rel.Version != "0.5.1" {
			t.Errorf("%s: Version = %q, want 0.5.1", tc.platform, rel.Version)
		}
		if rel.Metadata["appcast.enclosure.url"] != tc.wantURL {
			t.Errorf("%s: enclosure = %v, want %s", tc.platform, rel.Metadata["appcast.enclosure.url"], tc.wantURL)
		}
		if rel.Verification == nil || rel.Verification.SignatureAlgo != "ed25519" {
			t.Errorf("%s: Verification = %+v, want an ed25519 signature", tc.platform, rel.Verification)
		}
	}

	// Already-current clients must be told there is nothing to do.
	rel, err := prov.Check(t.Context(), updater.CheckRequest{
		CurrentVersion: "0.5.1", Platform: "darwin", Arch: "arm64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rel != nil {
		t.Errorf("Check on the current version returned %+v, want nil", rel)
	}
}

// A signature produced by `appcast sign` must verify against the digest the
// updater computes. Together with the provider test above this covers the full
// sign -> publish -> parse -> verify path.
func TestSignedArtifactVerifiesThroughFeed(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("artifact bytes")
	path := filepath.Join(t.TempDir(), "Knomit-0.5.1-darwin-arm64.app.zip")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	sig64, err := SignFile(priv, path)
	if err != nil {
		t.Fatal(err)
	}
	items := []Item{{
		Version: "0.5.1", Title: "0.5.1", OS: "darwin",
		URL: "https://example.test/a.zip", Length: int64(len(payload)),
		EdSignature: sig64, PublishedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
	}}
	feed, err := BuildFeed("https://example.test/appcast.xml", items)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(feed)
	}))
	defer srv.Close()

	prov, err := appcast.New(appcast.Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	rel, err := prov.Check(t.Context(), updater.CheckRequest{
		CurrentVersion: "0.5.0", Platform: "darwin", Arch: "arm64",
	})
	if err != nil || rel == nil {
		t.Fatalf("Check: rel=%v err=%v", rel, err)
	}

	sum := sha256.Sum256(payload)
	if !ed25519.Verify(pub, sum[:], rel.Verification.Signature) {
		t.Error("signature round-tripped through the feed does not verify")
	}
}

func TestItemsFromReleasesEmitsOnlyDarwinDesktopArtifacts(t *testing.T) {
	// Three exclusions at once:
	//   - Sidecars are themselves release assets and must never become items.
	//   - The server tarball is not a desktop artifact.
	//   - The AppImage is EXCLUDED even though it is signed, because Linux
	//     does not self-update: pkg/updater is AppImage-unaware and would
	//     swap the FUSE mount path instead of the .AppImage file. Publishing
	//     a linux item would break every installed AppImage.
	dir := t.TempDir()
	for _, name := range []string{
		"Knomit-0.5.1-darwin-arm64.app.zip.ed25519",
		"Knomit-0.5.1-linux-amd64.AppImage.ed25519",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("SIGDARWIN"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	releases := []byte(`[{
	  "tag_name": "v0.5.1",
	  "name": "0.5.1",
	  "body": "notes",
	  "draft": false,
	  "prerelease": false,
	  "published_at": "2026-07-28T12:00:00Z",
	  "assets": [
	    {"name": "Knomit-0.5.1-darwin-arm64.app.zip", "size": 10,
	     "browser_download_url": "https://example.test/mac.zip"},
	    {"name": "Knomit-0.5.1-darwin-arm64.app.zip.ed25519", "size": 3,
	     "browser_download_url": "https://example.test/mac.zip.ed25519"},
	    {"name": "Knomit-0.5.1-linux-amd64.AppImage", "size": 20,
	     "browser_download_url": "https://example.test/linux.AppImage"},
	    {"name": "knomit-0.5.1-darwin-arm64.tar.gz", "size": 30,
	     "browser_download_url": "https://example.test/server.tar.gz"}
	  ]
	}]`)

	items, err := ItemsFromReleases(releases, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1 (darwin only — the AppImage is signed but must not be published)", len(items))
	}
	if items[0].OS != "darwin" || items[0].EdSignature != "SIGDARWIN" || items[0].Version != "0.5.1" {
		t.Errorf("item = %+v", items[0])
	}
}

// Attribute values must be XML-escaped, not Go-quoted. A raw `&` in an
// enclosure URL makes the whole document malformed, so the failure is not a
// bad entry — it is a feed that fails to parse for every client at once,
// silently retiring the update channel.
func TestBuildFeedEscapesAttributeValues(t *testing.T) {
	items := []Item{{
		Version: "0.5.1", Title: "0.5.1", OS: "darwin",
		URL:         "https://example.test/dl?name=Knomit&arch=arm64",
		Length:      1234,
		EdSignature: `sig"with<angle&amp>`,
		PublishedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
	}}

	out, err := BuildFeed("https://example.test/appcast.xml", items)
	if err != nil {
		t.Fatal(err)
	}

	// The whole point: it still parses.
	var parsed struct {
		XMLName xml.Name
		Items   []struct {
			Enclosure struct {
				URL string `xml:"url,attr"`
				Sig string `xml:"http://www.andymatuschak.org/xml-namespaces/sparkle edSignature,attr"`
			} `xml:"enclosure"`
		} `xml:"channel>item"`
	}
	if err := xml.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("generated feed is not well-formed XML: %v\n---\n%s", err, out)
	}
	if len(parsed.Items) != 1 {
		t.Fatalf("parsed %d items, want 1", len(parsed.Items))
	}
	// Round-trips to the ORIGINAL values, so escaping did not corrupt them.
	if got := parsed.Items[0].Enclosure.URL; got != items[0].URL {
		t.Errorf("url round-tripped as %q, want %q", got, items[0].URL)
	}
	if got := parsed.Items[0].Enclosure.Sig; got != items[0].EdSignature {
		t.Errorf("edSignature round-tripped as %q, want %q", got, items[0].EdSignature)
	}
	// Go quoting would have emitted a backslash escape; XML has no such thing.
	if strings.Contains(string(out), `\"`) {
		t.Errorf("feed contains a Go-style \\\" escape — %%q was used instead of escape()\n%s", out)
	}
}

// Two items for one (version, OS) are indistinguishable to wails'
// pickBestItem: it compares versions only and keeps the first of a tie, so
// which artifact every client downloads would come down to GitHub's asset
// ordering. Publishing that is worse than failing the release.
func TestBuildFeedRefusesTwoItemsForOnePlatform(t *testing.T) {
	pub := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	items := []Item{
		{
			Version: "0.5.1", Title: "0.5.1", OS: "darwin", EdSignature: "AAAA",
			URL: "https://example.test/Knomit-0.5.1-darwin-arm64.app.zip", PublishedAt: pub,
		},
		{
			Version: "0.5.1", Title: "0.5.1", OS: "darwin", EdSignature: "BBBB",
			URL: "https://example.test/Knomit-0.5.1-darwin-amd64.app.zip", PublishedAt: pub,
		},
	}

	if _, err := BuildFeed("https://example.test/appcast.xml", items); err == nil {
		t.Error("BuildFeed published two darwin items for one version; clients would get one at random")
	}

	// The same version on DIFFERENT platforms is fine — that is the normal
	// shape of a multi-platform release.
	items[1].OS = "linux"
	if _, err := BuildFeed("https://example.test/appcast.xml", items); err != nil {
		t.Errorf("BuildFeed rejected one item per platform: %v", err)
	}
}

// desktopArtifact keys on the full platform token, not just the extension.
// A bare ".app.zip" rule would make a future darwin-amd64 build eligible, and
// since the feed has no arch dimension every Mac would be served whichever of
// the two GitHub listed first.
func TestDesktopArtifactMatchesTheExactPlatform(t *testing.T) {
	if got := desktopArtifact("Knomit-0.5.1-darwin-arm64.app.zip"); got != "darwin" {
		t.Errorf("desktopArtifact(darwin-arm64) = %q, want darwin", got)
	}
	if got := desktopArtifact("Knomit-0.5.1-darwin-amd64.app.zip"); got != "" {
		t.Errorf("desktopArtifact(darwin-amd64) = %q, want \"\" — the feed cannot distinguish arches", got)
	}
	if got := desktopArtifact("Knomit-0.5.1-windows-amd64.app.zip"); got != "" {
		t.Errorf("desktopArtifact(windows) = %q, want \"\"", got)
	}
}

func TestDesktopArtifactExcludesAppImage(t *testing.T) {
	// Guarding the exclusion directly, not just via ItemsFromReleases: this is
	// the line to flip when the upstream $APPIMAGE fix lands and Linux gains
	// self-update. Until then a linux feed item is an outage, not a feature.
	if got := desktopArtifact("Knomit-0.5.1-linux-amd64.AppImage"); got != "" {
		t.Errorf("desktopArtifact(AppImage) = %q, want \"\" — linux does not self-update", got)
	}
	if got := desktopArtifact("Knomit-0.5.1-darwin-arm64.app.zip"); got != "darwin" {
		t.Errorf("desktopArtifact(app.zip) = %q, want darwin", got)
	}
	if got := desktopArtifact("Knomit-0.5.1-darwin-arm64.app.zip.ed25519"); got != "" {
		t.Errorf("desktopArtifact(sidecar) = %q, want \"\"", got)
	}
	if got := desktopArtifact("knomit-0.5.1-darwin-arm64.tar.gz"); got != "" {
		t.Errorf("desktopArtifact(server tarball) = %q, want \"\"", got)
	}
}

func TestItemsFromReleasesSkipsDraftsAndPrereleases(t *testing.T) {
	// dev-latest is a prerelease and must never enter the stable feed.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Knomit-0.9.9-darwin-arm64.app.zip.ed25519"), []byte("X"), 0o644); err != nil {
		t.Fatal(err)
	}
	releases := []byte(`[{
	  "tag_name": "dev-latest", "name": "dev", "draft": false, "prerelease": true,
	  "published_at": "2026-07-28T12:00:00Z",
	  "assets": [{"name": "Knomit-0.9.9-darwin-arm64.app.zip", "size": 1,
	              "browser_download_url": "https://example.test/x.zip"}]
	}, {
	  "tag_name": "v0.9.8", "name": "0.9.8", "draft": true, "prerelease": false,
	  "published_at": "2026-07-28T12:00:00Z",
	  "assets": [{"name": "Knomit-0.9.9-darwin-arm64.app.zip", "size": 1,
	              "browser_download_url": "https://example.test/y.zip"}]
	}]`)

	items, err := ItemsFromReleases(releases, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("got %d items, want 0 — drafts and prereleases must stay out of the stable feed", len(items))
	}
}

// An artifact whose sidecar is missing must be DROPPED, never published
// unsigned: runVerification returns nil for a release with no verification
// block, so an unsigned entry becomes an unauthenticated update channel.
func TestItemsFromReleasesDropsArtifactsWithNoSidecar(t *testing.T) {
	releases := []byte(`[{
	  "tag_name": "v0.5.1", "name": "0.5.1", "draft": false, "prerelease": false,
	  "published_at": "2026-07-28T12:00:00Z",
	  "assets": [{"name": "Knomit-0.5.1-darwin-arm64.app.zip", "size": 10,
	              "browser_download_url": "https://example.test/mac.zip"}]
	}]`)

	items, err := ItemsFromReleases(releases, t.TempDir()) // empty sig dir
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("got %+v, want no items — an unsigned artifact must be dropped, not published", items)
	}
}

func TestItemsFromReleasesOrdersNewestFirst(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []string{"0.5.1", "0.5.2"} {
		name := "Knomit-" + v + "-darwin-arm64.app.zip.ed25519"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("S"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	releases := []byte(`[{
	  "tag_name": "v0.5.1", "name": "0.5.1", "draft": false, "prerelease": false,
	  "published_at": "2026-07-01T12:00:00Z",
	  "assets": [{"name": "Knomit-0.5.1-darwin-arm64.app.zip", "size": 1,
	              "browser_download_url": "https://example.test/1.zip"}]
	}, {
	  "tag_name": "v0.5.2", "name": "0.5.2", "draft": false, "prerelease": false,
	  "published_at": "2026-07-28T12:00:00Z",
	  "assets": [{"name": "Knomit-0.5.2-darwin-arm64.app.zip", "size": 1,
	              "browser_download_url": "https://example.test/2.zip"}]
	}]`)

	items, err := ItemsFromReleases(releases, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Version != "0.5.2" {
		t.Errorf("first item is %s, want the newest (0.5.2)", items[0].Version)
	}
}

// The `v` prefix belongs to the git tag, not the version. The provider trims
// it defensively, but emitting it would make the feed disagree with what
// version.Version reports.
func TestItemsFromReleasesStripsTagPrefix(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Knomit-0.5.1-darwin-arm64.app.zip.ed25519"), []byte("S"), 0o644); err != nil {
		t.Fatal(err)
	}
	releases := []byte(`[{
	  "tag_name": "v0.5.1", "name": "", "draft": false, "prerelease": false,
	  "published_at": "2026-07-28T12:00:00Z",
	  "assets": [{"name": "Knomit-0.5.1-darwin-arm64.app.zip", "size": 1,
	              "browser_download_url": "https://example.test/1.zip"}]
	}]`)

	items, err := ItemsFromReleases(releases, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if items[0].Version != "0.5.1" {
		t.Errorf("Version = %q, want 0.5.1 without the v", items[0].Version)
	}
	if items[0].Title != "0.5.1" {
		t.Errorf("Title = %q, want the version as a fallback for an empty release name", items[0].Title)
	}
}

// A release that fails to contribute its own item must fail the run. The
// empty-feed guard cannot catch this: older releases keep supplying items, so
// the feed stays valid and non-empty while silently never offering the version
// just published.
func TestRequireVersion(t *testing.T) {
	items := []Item{
		{Version: "0.5.1", OS: "darwin", EdSignature: "A"},
		{Version: "0.5.0", OS: "darwin", EdSignature: "B"},
	}

	if err := RequireVersion(items, "0.5.1"); err != nil {
		t.Errorf("RequireVersion(0.5.1) = %v, want nil", err)
	}
	if err := RequireVersion(items, "0.5.2"); err == nil {
		t.Error("RequireVersion accepted a feed missing the version being released")
	}
	// A non-empty feed built entirely from OLDER releases is exactly the case
	// the empty-feed guard lets through.
	older := []Item{{Version: "0.5.0", OS: "darwin", EdSignature: "B"}}
	if err := RequireVersion(older, "0.5.1"); err == nil {
		t.Error("a feed of only older releases passed the check for 0.5.1")
	}
	// Empty disables the check, for regenerating the feed by hand.
	if err := RequireVersion(items, ""); err != nil {
		t.Errorf("RequireVersion with no version = %v, want nil", err)
	}
	if err := RequireVersion(nil, ""); err != nil {
		t.Errorf("RequireVersion(nil, \"\") = %v, want nil", err)
	}
}

// The fallback is what protects releases cut before the fence existed. The
// feed is rebuilt in full from the releases API on every stable release, so a
// fence-only rule would blank v0.5.1's <description> the next time we ship.
func TestExtractNotesFallsBackToTheWholeBody(t *testing.T) {
	body := "## Downloads\n\nno fence here"
	got, err := ExtractNotes(body)
	if err != nil {
		t.Fatal(err)
	}
	if got != body {
		t.Errorf("got %q, want the whole body", got)
	}
}

func TestExtractNotesReturnsOnlyTheFencedRegion(t *testing.T) {
	got, err := ExtractNotes(
		"<!-- appcast:begin -->\n## What's new\n\n- a thing\n<!-- appcast:end -->\n\n## Downloads\n| a | b |\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "## What's new") || !strings.Contains(got, "- a thing") {
		t.Errorf("fenced region lost content: %q", got)
	}
	if strings.Contains(got, "Downloads") {
		t.Errorf("content after the fence leaked in: %q", got)
	}
}

// An unterminated fence would otherwise swallow the entire body — install
// instructions and all — which is exactly the failure the fence exists to fix.
func TestExtractNotesRejectsAnUnterminatedFence(t *testing.T) {
	if _, err := ExtractNotes("<!-- appcast:begin -->\n## What's new\n\n## Downloads\n"); err == nil {
		t.Fatal("want an error for a fence with no closing marker")
	}
}

// A fence around nothing produces an empty <description>: a green run that
// tells every client nothing changed. Same class of silent failure as a
// release that misses its own feed entry.
func TestExtractNotesRejectsAnEmptyFencedRegion(t *testing.T) {
	if _, err := ExtractNotes("<!-- appcast:begin -->\n   \n<!-- appcast:end -->\n## Downloads\n"); err == nil {
		t.Fatal("want an error for an empty fenced region")
	}
}

// End to end through runFeed: a release whose sidecar never arrived must not
// produce a published feed file.
func TestRunFeedFailsWhenTheReleasedVersionIsMissing(t *testing.T) {
	dir := t.TempDir()
	sigs := filepath.Join(dir, "sigs")
	if err := os.MkdirAll(sigs, 0o755); err != nil {
		t.Fatal(err)
	}
	// Only the OLD release has a sidecar; 0.5.2's went missing.
	if err := os.WriteFile(filepath.Join(sigs, "Knomit-0.5.1-darwin-arm64.app.zip.ed25519"), []byte("S"), 0o644); err != nil {
		t.Fatal(err)
	}
	releasesPath := filepath.Join(dir, "releases.json")
	if err := os.WriteFile(releasesPath, []byte(`[{
	  "tag_name": "v0.5.1", "name": "0.5.1", "draft": false, "prerelease": false,
	  "published_at": "2026-07-01T12:00:00Z",
	  "assets": [{"name": "Knomit-0.5.1-darwin-arm64.app.zip", "size": 1,
	              "browser_download_url": "https://example.test/1.zip"}]
	}, {
	  "tag_name": "v0.5.2", "name": "0.5.2", "draft": false, "prerelease": false,
	  "published_at": "2026-07-28T12:00:00Z",
	  "assets": [{"name": "Knomit-0.5.2-darwin-arm64.app.zip", "size": 1,
	              "browser_download_url": "https://example.test/2.zip"}]
	}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "appcast.xml")
	args := []string{
		"-releases", releasesPath, "-sigs", sigs,
		"-link", "https://example.test/appcast.xml", "-out", out,
	}

	// Without -require-version this run is GREEN and publishes a feed that
	// never offers 0.5.2 — the silent failure being guarded against.
	if err := runFeed(args); err != nil {
		t.Fatalf("baseline runFeed: %v", err)
	}
	if err := os.Remove(out); err != nil {
		t.Fatal(err)
	}

	if err := runFeed(append(args, "-require-version", "0.5.2")); err == nil {
		t.Error("runFeed published a feed with no item for the version being released")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Error("a feed file was written despite the missing release")
	}

	// The version that IS present still passes.
	if err := runFeed(append(args, "-require-version", "0.5.1")); err != nil {
		t.Errorf("runFeed(-require-version 0.5.1) = %v, want nil", err)
	}
}
