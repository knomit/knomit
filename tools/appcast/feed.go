package main

import (
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// sparkleNS is the namespace wails' appcast parser binds its sparkle:* fields
// to. It must match exactly — a different URI parses as unnamespaced elements
// the provider ignores, producing a feed that looks fine and matches nothing.
const sparkleNS = "http://www.andymatuschak.org/xml-namespaces/sparkle"

// Item is one platform's download for one release.
type Item struct {
	Version     string
	Title       string
	Notes       string
	OS          string // "darwin" or "linux" — compared against runtime.GOOS
	URL         string
	Length      int64
	EdSignature string // base64, from `appcast sign`
	PublishedAt time.Time
}

// desktopArtifact maps a release asset filename to the GOOS it serves, or ""
// when the asset must not become a feed item (server tarballs, signature
// sidecars, checksums — and the Linux AppImage).
//
// The AppImage is deliberately excluded even though it ships signed. Linux
// does not self-update: pkg/updater is AppImage-unaware, so os.Executable()
// resolves into the FUSE mount and the swap would replace the mount path
// rather than the .AppImage file. Publishing a linux item here would break
// every installed AppImage, not merely fail to help it.
//
// When the upstream $APPIMAGE fix lands, adding `.AppImage -> "linux"` here
// and relaxing the GOOS guard in tools/desktop/update.go is the whole change.
// The two must move together.
func desktopArtifact(name string) string {
	switch {
	case strings.HasSuffix(name, sigSuffix):
		return ""
	case strings.HasSuffix(name, ".app.zip"):
		return "darwin"
	}
	return ""
}

// BuildFeed renders items as a Sparkle appcast. Element vs attribute placement
// is dictated by wails' parser (providers/appcast/appcast.go): sparkle:os and
// sparkle:shortVersionString are elements on <item>, sparkle:edSignature is an
// attribute on <enclosure>, and pubDate must parse as RFC1123Z or one of its
// siblings.
func BuildFeed(link string, items []Item) ([]byte, error) {
	var b strings.Builder
	b.WriteString(xml.Header)
	fmt.Fprintf(&b, "<rss version=\"2.0\" xmlns:sparkle=%q>\n", sparkleNS)
	b.WriteString("  <channel>\n")
	b.WriteString("    <title>knomit</title>\n")
	fmt.Fprintf(&b, "    <link>%s</link>\n", escape(link))
	b.WriteString("    <description>knomit desktop updates</description>\n")

	for _, it := range items {
		// Never emit an item we cannot authenticate: runVerification returns
		// nil for a release carrying no verification block, so an unsigned
		// entry here becomes an unauthenticated update for every client.
		if it.EdSignature == "" {
			return nil, fmt.Errorf("item %s/%s has no signature", it.Version, it.OS)
		}
		b.WriteString("    <item>\n")
		fmt.Fprintf(&b, "      <title>%s</title>\n", escape(it.Title))
		fmt.Fprintf(&b, "      <pubDate>%s</pubDate>\n", it.PublishedAt.UTC().Format(time.RFC1123Z))
		fmt.Fprintf(&b, "      <sparkle:version>%s</sparkle:version>\n", escape(it.Version))
		fmt.Fprintf(&b, "      <sparkle:shortVersionString>%s</sparkle:shortVersionString>\n", escape(it.Version))
		fmt.Fprintf(&b, "      <sparkle:os>%s</sparkle:os>\n", escape(it.OS))
		fmt.Fprintf(&b, "      <description><![CDATA[%s]]></description>\n",
			strings.ReplaceAll(it.Notes, "]]>", "]]&gt;"))
		fmt.Fprintf(&b,
			"      <enclosure url=%q length=%q type=\"application/octet-stream\" sparkle:edSignature=%q />\n",
			it.URL, fmt.Sprint(it.Length), it.EdSignature)
		b.WriteString("    </item>\n")
	}

	b.WriteString("  </channel>\n</rss>\n")
	return []byte(b.String()), nil
}

func escape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// ghRelease is the subset of the GitHub releases API response the feed needs.
type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// ItemsFromReleases turns a GitHub releases API payload into feed items,
// reading each artifact's detached signature from sigDir.
//
// Two exclusions are deliberate and load-bearing:
//   - Drafts and prereleases are skipped, so the rolling `dev-latest`
//     pre-release never enters the stable feed.
//   - An artifact whose sidecar is missing is DROPPED, not published
//     unsigned. pkg/updater installs a release carrying no verification
//     block without complaint, so an unsigned entry here would silently
//     become an unauthenticated update for every client.
func ItemsFromReleases(releasesJSON []byte, sigDir string) ([]Item, error) {
	var releases []ghRelease
	if err := json.Unmarshal(releasesJSON, &releases); err != nil {
		return nil, fmt.Errorf("parse releases json: %w", err)
	}

	var items []Item
	for _, rel := range releases {
		if rel.Draft || rel.Prerelease {
			continue
		}
		version := strings.TrimPrefix(rel.TagName, "v")
		for _, a := range rel.Assets {
			goos := desktopArtifact(a.Name)
			if goos == "" {
				continue
			}
			sig, err := os.ReadFile(filepath.Join(sigDir, a.Name+sigSuffix))
			if err != nil {
				fmt.Fprintf(os.Stderr, "appcast: skipping %s — no signature sidecar\n", a.Name)
				continue
			}
			title := rel.Name
			if title == "" {
				title = version
			}
			items = append(items, Item{
				Version:     version,
				Title:       title,
				Notes:       rel.Body,
				OS:          goos,
				URL:         a.URL,
				Length:      a.Size,
				EdSignature: strings.TrimSpace(string(sig)),
				PublishedAt: rel.PublishedAt,
			})
		}
	}

	// Newest first. The provider scans every item regardless, but a human
	// opening the feed should see the current release at the top.
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].PublishedAt.After(items[j].PublishedAt)
	})
	return items, nil
}

func runFeed(args []string) error {
	fs := flag.NewFlagSet("feed", flag.ExitOnError)
	releasesPath := fs.String("releases", "", "path to GitHub releases API JSON")
	sigDir := fs.String("sigs", ".", "directory holding the .ed25519 sidecars")
	link := fs.String("link", "", "public URL of the published feed")
	out := fs.String("out", "appcast.xml", "output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *releasesPath == "" || *link == "" {
		return fmt.Errorf("feed needs -releases and -link")
	}

	raw, err := os.ReadFile(*releasesPath)
	if err != nil {
		return err
	}
	items, err := ItemsFromReleases(raw, *sigDir)
	if err != nil {
		return err
	}
	// An empty feed would tell every client it is up to date, silently
	// retiring the update channel. Fail the release run instead.
	if len(items) == 0 {
		return fmt.Errorf("no signed desktop artifacts found — refusing to publish an empty feed")
	}
	feed, err := BuildFeed(*link, items)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, feed, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d items)\n", *out, len(items))
	return nil
}
