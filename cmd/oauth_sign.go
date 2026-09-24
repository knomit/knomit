package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"knomit/internal/oauth"
	"knomit/internal/pki"
)

// Consent path 2 on the operator's side (F19 phase 3b, Task 3). `knomit
// oauth approve|deny <id> --sign <issuer>` runs on the machine that holds
// the fleet master key — never on the instance — and prints a signed
// statement; `knomit oauth approve --signed <file|->` delivers it from any
// machine (so does curl: it is one POST of that JSON to <issuer>/oauth/approve).

type signOpts struct {
	verb, issuer, dir, instance, passFile, subject string
	scopes                                         []string
	scopesGiven, yes                               bool
}

var fingerprintArgRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// oauthSign fetches the waiting request's description, prints it for the
// operator to read (errOut), checks the instance against the master's own
// logs, signs, and writes the statement JSON to out.
func oauthSign(cmd *cobra.Command, id string, o signOpts) error {
	ctx := cmd.Context()
	errOut := cmd.ErrOrStderr()
	if o.dir == "" {
		return errors.New("--dir (the master directory) is required with --sign")
	}
	if !fingerprintArgRE.MatchString(o.instance) {
		return errors.New("--instance must be the instance's full fingerprint (64 lowercase hex, as `knomit identity show` prints it)")
	}
	// W2: the instance comes from the operator and is checked against what
	// THIS master issued and revoked — never taken from the description,
	// which anyone relaying the request could have written.
	host, err := issuedStanding(o.dir, o.instance)
	if err != nil {
		return err
	}
	if o.passFile != "-" && !o.yes {
		return errors.New("refusing to sign unattended: with --passphrase-file <file> add --yes, or use --passphrase-file - so that typing the passphrase after reading the request is the confirmation")
	}
	issuer := strings.TrimSuffix(o.issuer, "/")
	d, err := fetchDescription(ctx, issuer, id)
	if err != nil {
		return err
	}
	// The digest the operator signs is computed HERE from the fields printed
	// below; a description whose own digest disagrees is refused rather than
	// signed. The instance recomputes it from its own row in turn.
	digest := oauth.RequestDigest(oauth.Pending{ClientID: d.ClientID, ClientName: d.ClientName,
		RedirectURI: d.RedirectURI, Resource: d.Resource, Scopes: d.Scopes})
	if digest != d.Digest || d.ID != id {
		return fmt.Errorf("the description from %q is inconsistent (its digest does not cover its own fields); not signing", issuer)
	}

	stmt := oauth.ApprovalStatement{Verb: o.verb, Instance: o.instance, ID: id, Digest: digest,
		Expires: time.Now().Add(10 * time.Minute).Unix()}
	if o.verb == oauth.VerbApprove {
		stmt.Subject = o.subject
		stmt.Scopes = o.scopes
		if !o.scopesGiven {
			stmt.Scopes = oauth.DefaultCeiling(d.Scopes)
		}
	}
	if _, err := stmt.Bytes(); err != nil {
		return err
	}
	printDescription(errOut, d, stmt, host)

	if o.passFile == "-" {
		fmt.Fprintln(errOut, "Type the master passphrase to sign this, or interrupt to abort:")
	}
	pass, err := readPassphrase(cmd, o.passFile)
	if err != nil {
		return err
	}
	root, err := pki.LoadRoot(o.dir, pass)
	if err != nil {
		return err
	}
	key, ok := root.Signer.(ed25519.PrivateKey)
	if !ok {
		return fmt.Errorf("the master key is %T, not Ed25519", root.Signer)
	}
	ss, err := oauth.SignStatement(stmt, key)
	if err != nil {
		return err
	}
	ss.Issuer = issuer
	b, err := json.MarshalIndent(ss, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s\n", b)
	fmt.Fprintf(errOut, "signed; deliver it from any machine with: knomit oauth approve --signed <file>  (valid until %s)\n",
		time.Unix(stmt.Expires, 0).Format(time.RFC3339))
	return nil
}

// printDescription is the consent screen of path 2. Every requester-supplied
// field is %q-quoted, exactly as `knomit oauth pending` prints them, so a
// control, format or bidi character in a client_name reaches the terminal
// as an escape, never as itself. The redirect HOST is first: it is where
// the code will go, and a URL client id proves only who owns a domain.
func printDescription(w io.Writer, d oauth.Description, s oauth.ApprovalStatement, host string) {
	redirectHost := d.RedirectURI
	if u, err := url.Parse(d.RedirectURI); err == nil && u.Host != "" {
		redirectHost = u.Host
	}
	fmt.Fprintf(w, "About to sign: %s request %q on instance %q (%s…)\n", strings.ToUpper(s.Verb), d.ID, host, s.Instance[:8])
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  code goes to\t%q\n", redirectHost)
	fmt.Fprintf(tw, "  redirect\t%q\n", d.RedirectURI)
	fmt.Fprintf(tw, "  client\t%q (%q)\n", d.ClientName, d.ClientID)
	fmt.Fprintf(tw, "  resource\t%q\n", d.Resource)
	fmt.Fprintf(tw, "  requested\t%q\n", strings.Join(d.Scopes, " "))
	if s.Verb == oauth.VerbApprove {
		fmt.Fprintf(tw, "  grant\tsubject %q, scopes %q\n", s.Subject, strings.Join(s.Scopes, " "))
	}
	fmt.Fprintf(tw, "  request expires\t%s\n", d.ExpiresAt.Local().Format(time.RFC3339))
	_ = tw.Flush()
}

func fetchDescription(ctx context.Context, issuer, id string) (oauth.Description, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/oauth/pending/"+url.PathEscape(id), nil)
	if err != nil {
		return oauth.Description{}, err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return oauth.Description{}, fmt.Errorf("fetch the request's description from %q: %w", issuer, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return oauth.Description{}, fmt.Errorf("%q answered %d for request %q: %s", issuer, resp.StatusCode, id, fromIssuer(body))
	}
	var d oauth.Description
	if err := json.Unmarshal(body, &d); err != nil {
		return oauth.Description{}, fmt.Errorf("the description from %q is not JSON: %w", issuer, err)
	}
	return d, nil
}

// issuedStanding returns the host label of the instance whose fingerprint is
// fp, if this master issued it a certificate that is not revoked.
func issuedStanding(dir, fp string) (string, error) {
	issued, err := readIssued(dir)
	if err != nil {
		return "", err
	}
	revoked, err := pki.LoadRevoked(dir)
	if err != nil {
		return "", err
	}
	gone := map[string]bool{}
	for _, r := range revoked {
		gone[r.Serial.Text(16)] = true
	}
	seen := false
	for _, rec := range issued {
		if rec.Fingerprint != fp {
			continue
		}
		seen = true
		if !gone[strings.ToLower(rec.Serial)] {
			return strings.TrimPrefix(rec.SAN, "knomit://instance/"), nil
		}
	}
	if seen {
		return "", fmt.Errorf("instance %s… is revoked in %s; not signing for it", fp[:8], pki.RevokedLogFile)
	}
	return "", fmt.Errorf("instance %s… was never issued a certificate by the master in %s; not signing for it", fp[:8], dir)
}

// readIssued parses the master's issuance log.
func readIssued(dir string) ([]pki.Issued, error) {
	f, err := os.Open(filepath.Join(dir, pki.IssuedLogFile))
	if err != nil {
		return nil, fmt.Errorf("issuance log: %w", err)
	}
	defer f.Close()
	var out []pki.Issued
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var rec pki.Issued
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("%s: %w", pki.IssuedLogFile, err)
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

// oauthDeliver POSTs a signed statement (a file, or - for stdin) to the
// issuer it names, or to issuerFlag when given.
func oauthDeliver(cmd *cobra.Command, src, issuerFlag string) error {
	var raw []byte
	var err error
	if src == "-" {
		raw, err = io.ReadAll(io.LimitReader(cmd.InOrStdin(), 64<<10))
	} else {
		raw, err = os.ReadFile(src)
	}
	if err != nil {
		return fmt.Errorf("read the signed statement: %w", err)
	}
	var ss oauth.SignedStatement
	if err := json.Unmarshal(raw, &ss); err != nil {
		return fmt.Errorf("the signed statement is not JSON: %w", err)
	}
	issuer := strings.TrimSuffix(issuerFlag, "/")
	if issuer == "" {
		issuer = strings.TrimSuffix(ss.Issuer, "/")
	}
	if issuer == "" {
		return errors.New("the statement names no issuer; pass --issuer <url>")
	}
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, issuer+"/oauth/approve", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("deliver to %q: %w", issuer, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%q refused the statement (%d): %s", issuer, resp.StatusCode, fromIssuer(body))
	}
	var r struct{ ID, Decision string }
	_ = json.Unmarshal(body, &r)
	// The decision and id come from whoever answered at issuer, which for a
	// blob is its UNSIGNED issuer field: quoted, and next to the URL it was
	// actually posted to, so a rewritten issuer cannot pass for the instance.
	fmt.Fprintf(cmd.OutOrStdout(), "%q answered %q for request %q\n", issuer, r.Decision, r.ID)
	return nil
}

// fromIssuer renders bytes received from an issuer for a terminal (review
// B1). In path 2's threat model every one of them is attacker-chosen — the
// issuer URL reaches the operator from whoever relayed the request, and a
// --signed blob's issuer field is unsigned — and they are printed on the
// machine that holds the fleet master key. So they are %q-quoted, which turns
// every control, format and bidi character (ESC, OSC, U+202E, invalid UTF-8)
// into an escape, and capped.
func fromIssuer(b []byte) string {
	const maxShown = 512
	s := strings.TrimSpace(string(b))
	if len(s) > maxShown {
		return strconv.Quote(s[:maxShown]) + "…"
	}
	return strconv.Quote(s)
}
