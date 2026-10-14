package cmd

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"knomit/internal/config"
	"knomit/internal/pki"
	"knomit/internal/repos"
)

// identityCmd is `knomit identity`: the offline enrollment flow of F19
// phase 2. The fleet root side (init-root, enroll, revoke) runs on the
// machine whose fleet root directory (--dir, default <home>/fleet) holds the
// plain root.key; the instance side (install, show) runs on each instance against its own
// home. The fleet root SIGNS existing instance keys and never generates them;
// there is no CSR — enroll takes the instance's public key line.
func identityCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "identity",
		Short: "Instance certificates: mint the fleet root, enroll, install, revoke, show",
	}
	c.AddCommand(identityInitRootCmd(), identityEnrollCmd(), identityInstallCmd(), identityRevokeCmd(), identityShowCmd(),
		identityRootFingerprintCmd())
	return c
}

// crlValidityDefault is how far ahead a newly issued CRL's NextUpdate is
// set. It is an operator-tunable default, not a property of any fleet: a CRL
// past NextUpdate is still ENFORCED (with a warning), so this only decides
// when instances start nagging for a fresh one.
const crlValidityDefault = 30 * 24 * time.Hour

// fleetDirName is the default fleet root directory under the knomit home:
// the root key (root.key, a plain owner-only file, like an SSH key), the root
// certificate, the CRL and the two ledgers.
const fleetDirName = "fleet"

// resolveFleetRootDir is --dir, or <home>/fleet when it is not given.
func resolveFleetRootDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return "", fmt.Errorf("--dir not given and the knomit home is unresolved: %w", err)
	}
	return filepath.Join(cfg.Home, fleetDirName), nil
}

func identityInitRootCmd() *cobra.Command {
	var dir, cn string
	var validity, crlValidity time.Duration
	c := &cobra.Command{
		Use:   "init-root",
		Short: "Create the fleet root; its key is a plain owner-only file in the fleet root directory, like an SSH key",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cn == "" {
				return errors.New("--cn is required")
			}
			dir, err := resolveFleetRootDir(dir)
			if err != nil {
				return err
			}
			root, err := pki.NewRoot(dir, cn, validity)
			if err != nil {
				return err
			}
			// An empty CRL, Number 1, so the first bundle carries one: an
			// instance without a CRL refuses every peer (fail closed).
			if _, err := pki.IssueCRL(dir, root, nil, big.NewInt(1), time.Now().Add(crlValidity)); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "# fleet root created in %s; its key is %s\n", dir, filepath.Join(dir, pki.RootKeyFile))
			fmt.Fprintln(out, "# that file IS the fleet: whoever can read it can sign for the fleet; keep it private and back the directory up (losing it ends the fleet)")
			// The value each instance confirms with `identity install --root`.
			if id, err := pki.RootID(root.Cert); err == nil {
				fmt.Fprintf(out, "# fleet root fingerprint: %s (give it to each instance for `knomit identity install --root`)\n", id)
			}
			_, err = out.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Cert.Raw}))
			return err
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "fleet root directory to create (root.key, root.crt, crl.pem, the ledgers; default <home>/fleet)")
	c.Flags().StringVar(&cn, "cn", "", "root common name, e.g. knomit-fleet-root-<you>")
	c.Flags().DurationVar(&validity, "validity", 10*365*24*time.Hour, "root certificate validity")
	c.Flags().DurationVar(&crlValidity, "crl-validity", crlValidityDefault, "NextUpdate of the initial CRL")
	return c
}

// parseInstancePubkey reads an authorized_keys line (the instance's
// id_ed25519.pub, or the line `knomit serve` prints at first start) from a
// file path or inline, and returns the key. Its comment is ignored: the
// certificate's identity is the agent id the operator names (--agent-id),
// never a name read off the key line (D18).
func parseInstancePubkey(arg string) (ed25519.PublicKey, error) {
	line := []byte(arg)
	if b, err := os.ReadFile(arg); err == nil {
		line = b
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey(line)
	if err != nil {
		return nil, fmt.Errorf("--pubkey: not an authorized_keys line or a file holding one: %w", err)
	}
	cpk, ok := pk.(ssh.CryptoPublicKey)
	if !ok {
		return nil, errors.New("--pubkey: unsupported key")
	}
	pub, ok := cpk.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("--pubkey: %s key, want ssh-ed25519", pk.Type())
	}
	return pub, nil
}

// enrollEmail is the email SAN `identity enroll` issues: <agent id>@agents.knomit.io
// for an instance (the address it commits as), else the operator's address,
// or "" for an operator certificate with none.
func enrollEmail(agentID string, operator bool, email string) (string, error) {
	switch {
	case agentID != "" && operator:
		return "", errors.New("--agent-id and --operator are exclusive: an instance certificate names its agent id, an operator certificate does not")
	case agentID != "":
		if email != "" {
			return "", errors.New("--email is for an operator certificate; an instance certificate's address is <agent id>@" + pki.AgentEmailDomain)
		}
		return pki.AgentEmail(agentID)
	case !operator:
		return "", errors.New("--agent-id is required (the instance's agent id, as its member record's agent: names it), or --operator for an operator certificate")
	case pki.AgentIDOfEmail(email) != "":
		return "", fmt.Errorf("--email %q is an agent address: that is an instance certificate, issue it with --agent-id", email)
	}
	return email, nil
}

func identityEnrollCmd() *cobra.Command {
	var dir, pubArg, agentID, email, outPath string
	var operator bool
	var validity time.Duration
	c := &cobra.Command{
		Use:   "enroll",
		Short: "Sign an instance's EXISTING public key into a certificate bundle (fleet root side)",
		Long: `Sign an EXISTING public key into a certificate bundle (fleet root side).

The certificate's identity is its email address. An instance certificate
(--agent-id <id>) carries <id>@agents.knomit.io, the address the instance
commits as; it holds read and push:own implicitly. An operator certificate
(--operator, optionally --email <address>) carries any other address, or none;
it holds only the grants rows of its principal.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if pubArg == "" {
				return errors.New("--pubkey is required")
			}
			san, err := enrollEmail(agentID, operator, email)
			if err != nil {
				return err
			}
			dir, err := resolveFleetRootDir(dir)
			if err != nil {
				return err
			}
			pub, err := parseInstancePubkey(pubArg)
			if err != nil {
				return err
			}
			root, err := pki.LoadRoot(dir)
			if err != nil {
				return err
			}
			certPEM, rec, err := pki.IssueInstance(dir, root, pub, san, validity)
			if err != nil {
				return err
			}
			crlPEM, err := os.ReadFile(filepath.Join(dir, pki.CRLFile))
			if err != nil {
				return fmt.Errorf("current CRL: %w", err)
			}
			// The bundle: instance certificate, root certificate, current
			// CRL, in that order, each in its standard PEM type.
			var bundle bytes.Buffer
			bundle.Write(certPEM)
			bundle.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Cert.Raw}))
			bundle.Write(crlPEM)
			w := cmd.OutOrStdout()
			if outPath != "" {
				if err := os.WriteFile(outPath, bundle.Bytes(), 0o644); err != nil {
					return err
				}
			} else if _, err := w.Write(bundle.Bytes()); err != nil {
				return err
			}
			kind := "instance"
			if agentID == "" {
				kind = "operator"
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "enrolled email=%q serial=%s principal=%s:%s@cert not_after=%s\n",
				rec.Email, rec.Serial, kind, rec.Fingerprint, rec.NotAfter.Format(time.RFC3339))
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "fleet root directory (default <home>/fleet)")
	c.Flags().StringVar(&pubArg, "pubkey", "", "the instance's public key: a file (id_ed25519.pub) or the line itself")
	c.Flags().StringVar(&agentID, "agent-id", "", "issue an INSTANCE certificate for this agent id (email SAN <id>@"+pki.AgentEmailDomain+")")
	c.Flags().BoolVar(&operator, "operator", false, "issue an OPERATOR certificate instead (no agent id)")
	c.Flags().StringVar(&email, "email", "", "with --operator: the operator's email address for the certificate (optional)")
	c.Flags().StringVar(&outPath, "out", "", "write the bundle here instead of stdout")
	c.Flags().DurationVar(&validity, "validity", 90*24*time.Hour, "certificate validity")
	return c
}

func identityInstallCmd() *cobra.Command {
	var bundlePath string
	var opts installOpts
	c := &cobra.Command{
		Use:   "install",
		Short: "Install an enrollment bundle on THIS instance (instance side)",
		Long: `Install an enrollment bundle on THIS instance (instance side).

Before a first install, or a move to another fleet (--replace-root), the
bundle's fleet root fingerprint is printed and must be confirmed against the
fingerprint the fleet operator gave you out of band: with --root <fingerprint>,
by answering the prompt (only when --bundle is a file and stdin is a
terminal), or by --yes. A bundle under the root already installed (a renewal)
needs no confirmation.

The fingerprint --root takes is 64 hex characters (either case): the SHA-256
of the fleet root public key's SSH wire encoding, the same rule knomit uses
for an instance key's fingerprint. It is NOT what
` + "`openssl x509 -noout -fingerprint -sha256`" + ` prints: that hashes the
root certificate, not its key. Ways to get it:

  - the operator: ` + "`knomit identity init-root`" + ` prints it on its
    "# fleet root fingerprint:" line;
  - the operator, later: ` + "`knomit identity root-fingerprint`" + ` prints it
    from the fleet root directory's root.crt (the root key is not needed);
  - on the instance: ` + "`knomit identity install`" + ` without --root prints
    it as "bundle fleet root:" (compare it with the operator's value), and
    ` + "`knomit identity show`" + ` prints the installed root as "root:".`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			var raw []byte
			fromStdin := bundlePath == "" || bundlePath == "-"
			if fromStdin {
				raw, err = io.ReadAll(cmd.InOrStdin())
			} else {
				raw, err = os.ReadFile(bundlePath)
			}
			if err != nil {
				return err
			}
			// Stdin can answer a prompt only when it is a terminal and is not
			// already the bundle.
			opts.bundleOnStdin = fromStdin
			if !fromStdin && isTerminal(cmd.InOrStdin()) {
				opts.prompt = cmd.InOrStdin()
			}
			return installBundle(cmd.OutOrStdout(), cfg, raw, opts)
		},
	}
	c.Flags().StringVar(&bundlePath, "bundle", "", "bundle file from `knomit identity enroll` (default/-: stdin)")
	c.Flags().BoolVar(&opts.replaceRoot, "replace-root", false, "allow a bundle from a DIFFERENT fleet root (moves this instance to another fleet; the new root must still be confirmed)")
	c.Flags().StringVar(&opts.root, "root", "", "the fleet root fingerprint the operator gave you out of band (64 hex: SHA-256 of the root key's SSH encoding, NOT openssl's certificate fingerprint): confirms a bundle whose root matches it")
	c.Flags().BoolVar(&opts.yes, "yes", false, "trust the bundle's fleet root without confirming it")
	return c
}

// installOpts is how `identity install` was asked to confirm a fleet root.
type installOpts struct {
	replaceRoot bool
	root        string    // --root: the out-of-band fingerprint
	yes         bool      // --yes
	prompt      io.Reader // the terminal to ask on; nil when there is none
	// bundleOnStdin: the bundle was read from stdin, so stdin cannot also
	// answer a prompt.
	bundleOnStdin bool
}

// isTerminal reports whether r is a character device (a terminal). A pipe,
// a file or a test's reader is not. It needs no golang.org/x/term: this is
// only "may we ask", not raw-mode input.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// installBundle is `identity install` without the flag parsing, so tests can
// drive it. The checks and the writes are pki.InstallBundle's, the one
// implementation the desktop's Settings window also calls (knomit#256).
//
// A root this instance does not already trust — a first install, or a move
// with --replace-root — is shown and must be confirmed first (knomit#299).
// The installed root read here is what pki.InstallBundle must still find
// under its lock, so a concurrent install by another process is refused
// rather than overwritten.
func installBundle(out io.Writer, cfg config.Config, raw []byte, opts installOpts) error {
	dir, keyPath := cfg.TLS.Dir, config.ResolveKeyPath(cfg)
	preview, err := pki.PreviewBundle(keyPath, raw)
	if err != nil {
		return err
	}
	// An installed root.crt that does not load is refused HERE, before the
	// root is shown or asked about.
	installed, err := pki.InstalledRoot(dir)
	if err != nil {
		return err
	}
	switch {
	case installed.Fingerprint == preview.Root.Fingerprint:
		// A renewal: the root is the one this instance already trusts.
	case installed.Fingerprint != "" && !opts.replaceRoot:
		return fmt.Errorf("this instance is enrolled under root %q and the bundle is from a different root %q; pass --replace-root to move it to the other fleet",
			installed.CommonName, preview.Root.CommonName)
	default:
		if err := confirmRoot(out, preview, installed, opts); err != nil {
			return err
		}
	}
	id, err := pki.InstallBundle(dir, keyPath, raw, pki.InstallOptions{ExpectInstalledRoot: installed.Fingerprint})
	var rd *pki.RootDiffersError
	if errors.As(err, &rd) {
		return fmt.Errorf("the installed fleet root changed while this command ran (was %s, now %s); nothing was installed — run it again",
			orNone(installed.Fingerprint), orNone(rd.Installed.Fingerprint))
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "installed in %s\nprincipal: %s:%s@cert\nemail: %q\nnot_after: %s\n",
		dir, id.PrincipalKind(), id.Fingerprint, id.Email, id.NotAfter.Format(time.RFC3339))
	if cfg.TLS.Addr == "" {
		fmt.Fprintln(out, "the TLS listener is off; to accept enrolled peers add to knomit.toml:\n  [tls]\n  addr = \"0.0.0.0:19279\"")
	} else {
		fmt.Fprintln(out, "a running `knomit serve` picks this up within its recheck interval or on its next handshake; a stopped one on start")
	}
	return nil
}

func orNone(fp string) string {
	if fp == "" {
		return "none"
	}
	return fp
}

// rootFingerprintForm is the one form --root takes, and where to get it: the
// help text, the refusal of a malformed value and the fleet docs say the same.
const rootFingerprintForm = "the fleet root fingerprint: 64 hex characters, the SHA-256 of the root " +
	"public key's SSH wire encoding (the same rule as an instance key's fingerprint). Get it from the operator, who " +
	"has it from `knomit identity init-root` (its `# fleet root fingerprint:` line) or can print it again with " +
	"`knomit identity root-fingerprint` (read from the fleet root directory's root.crt); on an " +
	"instance, `knomit identity install` without --root prints it as `bundle fleet root:` and " +
	"`knomit identity show` prints the installed one as `root:`"

// rootFingerprintRe is the form --root takes once trimmed and lowercased.
var rootFingerprintRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// parseRootFingerprint normalises --root (surrounding space, case) and
// refuses anything that is not the 64-hex root fingerprint, naming the form
// and where to get it. A value that looks like `openssl x509 -fingerprint`
// output is called out: that hashes the root CERTIFICATE, not its key, so it
// can never match, whatever its format.
func parseRootFingerprint(s string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	if rootFingerprintRe.MatchString(v) {
		return v, nil
	}
	hint := ""
	if strings.Contains(v, "fingerprint=") || strings.Contains(v, ":") {
		hint = " (this looks like `openssl x509 -fingerprint` output, which hashes the root CERTIFICATE, not its key: a different value)"
	}
	return "", fmt.Errorf("--root %q is not a fleet root fingerprint%s; --root takes %s; nothing was installed", s, hint, rootFingerprintForm)
}

// confirmRoot shows the bundle's root and principal and returns nil only if
// the user confirmed that root: --root naming it, --yes, or "y" at the
// prompt. Without any of them it refuses, naming the fingerprint and the
// flags, so a script sees why.
func confirmRoot(out io.Writer, p pki.BundlePreview, installed pki.RootInfo, opts installOpts) error {
	if installed.Fingerprint != "" {
		fmt.Fprintf(out, "installed fleet root: %s (%q)\n", installed.Fingerprint, installed.CommonName)
	}
	fmt.Fprintf(out, "bundle fleet root: %s (%q)\nprincipal: %s\n", p.Root.Fingerprint, p.Root.CommonName, p.Principal)
	switch {
	case opts.root != "":
		given, err := parseRootFingerprint(opts.root)
		if err != nil {
			return err
		}
		if given != p.Root.Fingerprint {
			return fmt.Errorf("--root %s is not the bundle's fleet root %s; nothing was installed", opts.root, p.Root.Fingerprint)
		}
		return nil
	case opts.yes:
		return nil
	case opts.prompt != nil:
		fmt.Fprint(out, "Is this the fleet root fingerprint the operator gave you? [y/N] ")
		line, err := bufio.NewReader(opts.prompt).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
			return nil
		}
		return errors.New("the fleet root was not confirmed; nothing was installed")
	}
	why := ""
	if opts.bundleOnStdin {
		why = " (no prompt: stdin is the bundle; pass it as --bundle <file> on a terminal to be asked)"
	}
	return fmt.Errorf("the bundle's fleet root %s is not confirmed%s: compare it with the fingerprint the fleet operator gave you, then rerun with --root %s (or --yes to skip the check); nothing was installed",
		p.Root.Fingerprint, why, p.Root.Fingerprint)
}

func identityRevokeCmd() *cobra.Command {
	var dir, serialHex, fp, reason string
	var crlValidity time.Duration
	c := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke a certificate by serial or by fingerprint and reissue the CRL (fleet root side)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (serialHex == "") == (fp == "") {
				return errors.New("exactly one of --serial or --fingerprint is required")
			}
			dir, err := resolveFleetRootDir(dir)
			if err != nil {
				return err
			}
			serials, err := serialsToRevoke(dir, serialHex, fp)
			if err != nil {
				return err
			}
			root, err := pki.LoadRoot(dir)
			if err != nil {
				return err
			}
			var num *big.Int
			for _, s := range serials {
				n, err := pki.Revoke(dir, root, pki.Revoked{Serial: s, At: time.Now().UTC(), Reason: reason}, time.Now().Add(crlValidity))
				// --fingerprint names every serial the key was ever issued:
				// one revoked earlier is skipped, so the live ones still are.
				// --serial names one, and re-revoking it stays an error.
				if fp != "" && errors.Is(err, pki.ErrAlreadyRevoked) {
					fmt.Fprintf(cmd.OutOrStdout(), "serial %s is already revoked\n", s.Text(16))
					continue
				}
				if err != nil {
					return err
				}
				num = n
				fmt.Fprintf(cmd.OutOrStdout(), "revoked serial %s\n", s.Text(16))
			}
			if num == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "every certificate for %s is already revoked; nothing new to revoke, %s is unchanged\n", fp, filepath.Join(dir, pki.CRLFile))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "CRL %s is now Number %s; copy it to every instance's [tls].dir\n", filepath.Join(dir, pki.CRLFile), num)
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "fleet root directory (default <home>/fleet)")
	c.Flags().StringVar(&serialHex, "serial", "", "serial to revoke (hex, as enroll printed it)")
	c.Flags().StringVar(&fp, "fingerprint", "", "revoke EVERY certificate issued for this 64-hex fingerprint")
	c.Flags().StringVar(&reason, "reason", "", "free text recorded in revoked.jsonl")
	c.Flags().DurationVar(&crlValidity, "crl-validity", crlValidityDefault, "NextUpdate of the reissued CRL")
	return c
}

// serialsToRevoke resolves --serial or --fingerprint against issued.jsonl.
// A serial the fleet root never issued is refused: a typo must not quietly
// revoke nothing.
func serialsToRevoke(dir, serialHex, fp string) ([]*big.Int, error) {
	issued, err := readIssued(dir)
	if err != nil {
		return nil, err
	}
	var out []*big.Int
	for _, rec := range issued {
		if (serialHex != "" && strings.EqualFold(rec.Serial, strings.TrimPrefix(serialHex, "0x"))) || (fp != "" && rec.Fingerprint == fp) {
			s, ok := new(big.Int).SetString(rec.Serial, 16)
			if !ok {
				return nil, fmt.Errorf("%s: bad serial %q", pki.IssuedLogFile, rec.Serial)
			}
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no certificate in issued.jsonl matches; nothing revoked")
	}
	return out, nil
}

func identityShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show this instance's agent id, fingerprint, certificate and CRL state",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			return showIdentity(cmd.OutOrStdout(), cfg)
		},
	}
}

func showIdentity(out io.Writer, cfg config.Config) error {
	st, err := pki.Status(cfg.TLS.Dir, config.ResolveKeyPath(cfg))
	if err != nil {
		return err
	}
	agentID, found, err := repos.StoredAgentID(cfg.Home)
	switch {
	case err != nil:
		fmt.Fprintf(out, "agent_id: unreadable: %v\n", err)
	case !found:
		fmt.Fprintln(out, "agent_id: none (assigned when the instance first starts)")
	default:
		// A stored id that is no longer valid (upper case, from before agent
		// ids were lower case) is shown as stored, with the reason boot will
		// refuse it and the remedy. show never rewrites it.
		if verr := pki.ValidAgentID(agentID); verr != nil {
			fmt.Fprintf(out, "agent_id: %s (invalid: %v; boot refuses it: %v)\n", agentID, verr, repos.ErrStoredAgentIDInvalid)
		} else {
			fmt.Fprintf(out, "agent_id: %s\n", agentID)
		}
	}
	fmt.Fprintf(out, "key: %s\nfingerprint: %s\nshort: %s\n", st.KeyPath, st.Fingerprint, pki.Short(st.Fingerprint))
	switch {
	case !st.Enrolled:
		fmt.Fprintf(out, "certificate: none (not enrolled; see `knomit identity install`)\n")
	case st.IdentityErr != nil:
		fmt.Fprintf(out, "certificate: unreadable identity: %v\n", st.IdentityErr)
	case st.Cert != nil:
		id := st.Cert
		fmt.Fprintf(out, "principal: %s:%s@cert\nemail: %q\nserial: %s\nnot_after: %s\n",
			id.PrincipalKind(), id.Fingerprint, id.Email, id.Serial.Text(16), id.NotAfter.Format(time.RFC3339))
	}
	if st.Root != nil {
		fmt.Fprintf(out, "root: %s (%q)\n", st.Root.Fingerprint, st.Root.CommonName)
	}
	if st.CRL != nil {
		fmt.Fprintf(out, "crl_number: %s\ncrl_next_update: %s\n", st.CRL.Number, st.CRL.NextUpdate.Format(time.RFC3339))
	}
	if cfg.TLS.Addr == "" {
		fmt.Fprintln(out, "tls_listener: off ([tls].addr is empty)")
	} else {
		fmt.Fprintf(out, "tls_listener: %s\n", cfg.TLS.Addr)
	}
	return nil
}

func identityRootFingerprintCmd() *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use:   "root-fingerprint",
		Short: "Print the fleet root fingerprint (the value `install --root` takes) from the fleet root directory's root.crt",
		Long: `Print the fleet root fingerprint: 64 hex characters, the SHA-256 of the
root public key's SSH wire encoding, read from <dir>/root.crt. It does not
need the root key, so it works on any machine that has the root certificate.
Give the value to each instance for
` + "`knomit identity install --root`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := resolveFleetRootDir(dir)
			if err != nil {
				return err
			}
			cert, err := pki.LoadRootCert(filepath.Join(dir, pki.RootCertFile))
			if err != nil {
				return err
			}
			id, err := pki.RootID(cert)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "fleet root directory holding root.crt (default <home>/fleet)")
	return c
}
