package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"sort"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/stretchr/testify/require"
)

// The advertisement carries both new capabilities, and knomit-repo-id carries
// the store's ACTUAL root commit — not merely some 40-hex string.
func TestGitHandler_AdvertisesSidebandAndRepoID(t *testing.T) {
	svc, srv := servedStore(t, 2)

	root, err := svc.RootCommit(context.Background(), "main")
	require.NoError(t, err)
	require.Len(t, root, 40)

	caps := advertisedCapabilities(t, srv.URL)
	require.Contains(t, caps, "side-band-64k")
	require.Contains(t, caps, "knomit-repo-id="+root,
		"the advertised identity must be the store's own root commit; caps=%q", caps)
}

// A store whose consensus branch has ADVANCED still advertises the same
// identity: the cache is keyed by the tip, so a new tip recomputes, and the
// answer is the same root because history only grows in front of it.
func TestGitHandler_RepoIDSurvivesNewCommits(t *testing.T) {
	svc, srv := servedStore(t, 1)
	ctx := context.Background()

	before := advertisedCapabilities(t, srv.URL)
	root, err := svc.RootCommit(ctx, "main")
	require.NoError(t, err)
	require.Contains(t, before, "knomit-repo-id="+root)

	tipBefore, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/later.md", testFactBody("later", 0.9, nil), "later", "")
	require.NoError(t, err)
	tipAfter, err := svc.Branches().HeadCommit(ctx, "main")
	require.NoError(t, err)
	require.NotEqual(t, tipBefore, tipAfter, "the fixture must actually move the tip")

	require.Contains(t, advertisedCapabilities(t, srv.URL), "knomit-repo-id="+root)
}

// Real git asked for progress must RENDER the band-2 lines, prefixed
// "remote: ". This is the client the user runs by hand.
func TestGitHandler_RealGitClonePrintsSidebandProgress(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	_, srv := servedStore(t, 3)

	dst := t.TempDir()
	cmd := exec.Command("git", "-c", "gc.auto=0", "clone", "--progress", srv.URL, dst+"/c")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	s := string(out)
	require.Contains(t, s, "remote: knomit: sending ", "clone stderr:\n%s", s)
	require.Contains(t, s, "remote: knomit: done", "clone stderr:\n%s", s)
}

// go-git with a Progress writer receives the same lines. This is knomit's own
// subscriber, and the whole reason the sideband exists.
func TestGitHandler_GoGitCloneWithProgressReceivesLines(t *testing.T) {
	_, srv := servedStore(t, 3)

	var progress bytes.Buffer
	_, err := gogit.Clone(memory.NewStorage(), nil, &gogit.CloneOptions{
		URL:      srv.URL,
		Progress: &progress,
	})
	require.NoError(t, err)

	s := progress.String()
	require.Contains(t, s, "knomit: sending ", "progress:\n%s", s)
	require.Contains(t, s, "knomit: done", "progress:\n%s", s)
	// The object count is real, not a placeholder.
	require.NotContains(t, s, "knomit: sending 0 objects")
}

// The SAME request, asked for twice with and without side-band-64k, must
// deliver the SAME packfile bytes — and must deliver them in two different
// shapes: raw straight after the NAK when sideband was not requested, framed
// on band 1 when it was.
//
// One test for both halves on purpose. Asserting the raw shape alone cannot
// tell "the muxer is correctly conditional" from "the muxer is broken and
// never runs", and asserting band 1 alone cannot tell a correct mux from one
// that also mangled the pack. Comparing the two responses' pack bytes to each
// other is what makes both claims falsifiable without pinning a packfile
// fixture that any encoder change would invalidate.
func TestGitHandler_SidebandIsConditionalAndLosslessOverThePack(t *testing.T) {
	svc, srv := servedStore(t, 4)
	tip, err := svc.Branches().HeadCommit(context.Background(), "main")
	require.NoError(t, err)

	const nak = "0008NAK\n"

	plain := postUploadPack(t, srv.URL, uploadPackRequestBody(t, tip, nil))
	require.True(t, bytes.HasPrefix(plain, []byte(nak)),
		"the ACK/NAK section is never muxed, got %s", firstBytes(plain, 32))
	plainPack := plain[len(nak):]
	require.True(t, bytes.HasPrefix(plainPack, []byte("PACK")),
		"without sideband the pack follows the NAK raw, got %s", firstBytes(plainPack, 32))

	framed := postUploadPack(t, srv.URL, uploadPackRequestBody(t, tip, []string{"side-band-64k"}))
	require.True(t, bytes.HasPrefix(framed, []byte(nak)))
	rest := framed[len(nak):]
	require.False(t, bytes.HasPrefix(rest, []byte("PACK")),
		"with sideband the pack must be inside band 1, got %s", firstBytes(rest, 32))

	var band1, band2 bytes.Buffer
	sawFlush := false
	scanner := pktline.NewScanner(bytes.NewReader(rest))
	for scanner.Scan() {
		b := scanner.Bytes()
		if len(b) == 0 {
			sawFlush = true
			continue
		}
		require.False(t, sawFlush, "nothing follows the terminating flush-pkt")
		switch b[0] {
		case 1:
			band1.Write(b[1:])
		case 2:
			band2.Write(b[1:])
		default:
			t.Fatalf("unexpected sideband channel %d", b[0])
		}
	}
	require.NoError(t, scanner.Err())
	require.True(t, sawFlush, "a sideband stream is terminated by a flush-pkt")

	// The object SET, not the bytes: the pack encoder does not promise a stable
	// object order between two encodes of the same set, so byte-equality would
	// be a flaky assertion about the encoder rather than a real one about the
	// muxer.
	require.Equal(t, packedObjects(t, plainPack), packedObjects(t, band1.Bytes()),
		"band 1 must carry the same objects the unframed response carries")
	require.NotEmpty(t, packedObjects(t, plainPack), "the fixture must actually pack something")
	require.Contains(t, band2.String(), "knomit: sending ")
	require.Contains(t, band2.String(), "knomit: done")
}

// advertisedCapabilities returns the capability list from the first ref line of
// /info/refs, read as raw bytes so an unknown capability is visible rather than
// dropped by a parser that only keeps the ones it knows.
func advertisedCapabilities(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.Get(base + "/info/refs?service=git-upload-pack")
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	scanner := pktline.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := string(scanner.Bytes())
		if strings.HasPrefix(line, "# service=") || strings.TrimSpace(line) == "" {
			continue
		}
		_, caps, found := strings.Cut(line, "\x00")
		require.True(t, found, "first ref line carries the capabilities: %q", line)
		return strings.TrimSpace(caps)
	}
	require.NoError(t, scanner.Err())
	t.Fatalf("no ref line in advertisement: %q", raw)
	return ""
}

// uploadPackRequestBody hand-builds a single-round upload-pack request for one
// want, with exactly the capabilities asked for. Hand-built because the point
// is to control whether side-band-64k is requested at all — go-git's client
// sets it whenever the server advertises it, so it cannot express the
// no-sideband case.
func uploadPackRequestBody(t *testing.T, want string, caps []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	e := pktline.NewEncoder(&buf)
	first := "want " + want
	if len(caps) > 0 {
		first += " " + strings.Join(caps, " ")
	}
	require.NoError(t, e.Encodef("%s\n", first))
	require.NoError(t, e.Flush())
	require.NoError(t, e.Encodef("done\n"))
	return buf.Bytes()
}

func postUploadPack(t *testing.T, base string, body []byte) []byte {
	t.Helper()
	resp, err := http.Post(base+"/git-upload-pack", "application/x-git-upload-pack-request", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return raw
}

// packedObjects unpacks a packfile and returns its object hashes, sorted.
func packedObjects(t *testing.T, pack []byte) []string {
	t.Helper()
	sto := memory.NewStorage()
	require.NoError(t, packfile.UpdateObjectStorage(sto, bytes.NewReader(pack)))
	iter, err := sto.IterEncodedObjects(plumbing.AnyObject)
	require.NoError(t, err)
	var hashes []string
	require.NoError(t, iter.ForEach(func(o plumbing.EncodedObject) error {
		hashes = append(hashes, o.Hash().String())
		return nil
	}))
	sort.Strings(hashes)
	return hashes
}

func firstBytes(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return fmt.Sprintf("%q", b)
}
