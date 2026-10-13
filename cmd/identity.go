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
)

// identityCmd is `knomit identity`: the offline enrollment flow of F19
// phase 2. The master side (init-master, enroll, revoke) runs on the
// operator's machine against a --dir that never lives on a fleet machine;
// the instance side (install, show) runs on each instance against its own
// home. The master SIGNS existing instance keys and never generates them;
// there is no CSR — enroll takes the instance's public key line.
func identityCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "identity",
		Short: "Instance certificates: mint the fleet root, enroll, install, revoke, show",
	}
	c.AddCommand(identityInitMasterCmd(), identityEnrollCmd(), identityInstallCmd(), identityRevokeCmd(), identityShowCmd(),
		identityExportRootCmd(), identityImportRootCmd(), identityRootFingerprintCmd())
	return c
}

// crlValidityDefault is how far ahead a newly issued CRL's NextUpdate is
// set. It is an operator-tunable default, not a property of any fleet: a CRL
// past NextUpdate is still ENFORCED (with a warning), so this only decides
// when instances start nagging for a fresh one.
const crlValidityDefault = 30 * 24 * time.Hour

// readPassphrase reads the master passphrase from path ("-" = the first line
// of stdin). There is no interactive prompt: reading a passphrase without
// echo needs golang.org/x/term, which this phase does not add as a
// dependency; a file (or a pipe) also keeps it out of shell history.
func readPassphrase(cmd *cobra.Command, path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("--passphrase-file is required (use - to read one line from stdin)")
	}
	var raw []byte
	var err error
	if path == "-" {
		line, rerr := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return nil, rerr
		}
		raw = []byte(line)
	} else if raw, err = os.ReadFile(path); err != nil {
		return nil, fmt.Errorf("read passphrase: %w", err)
	}
	p := bytes.TrimRight(raw, "\r\n")
	if len(p) == 0 {
		return nil, errors.New("the passphrase is empty")
	}
	return p, nil
}

// rootKeychain is where the master commands keep the fleet root key (D9).
// A variable so cmd tests can inject a fake; TestMain also points
// go-keyring at its in-memory mock, so no test reaches the real keychain.
var rootKeychain = pki.OSKeychain()

// masterDirName is the default master directory under the knomit home: the
// root certificate, the CRL and the two ledgers (the key itself is in the
// keychain, or root.key there when the keychain is unavailable).
const masterDirName = "master"

// resolveMasterDir is --dir, or <home>/master when it is not given.
func resolveMasterDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return "", fmt.Errorf("--dir not given and the knomit home is unresolved: %w", err)
	}
	return filepath.Join(cfg.Home, masterDirName), nil
}

// lazyPassphrase reads --passphrase-file only when custody needs it (the
// file fallback), and at most once.
func lazyPassphrase(cmd *cobra.Command, passFile string) pki.Passphrase {
	var p []byte
	var err error
	read := false
	return func() ([]byte, error) {
		if !read {
			read = true
			if passFile == "" {
				err = errors.New("the root key is not in the OS keychain here: --passphrase-file is required (use - to read one line from stdin)")
			} else {
				p, err = readPassphrase(cmd, passFile)
			}
		}
		return p, err
	}
}

// openMasterRoot opens the fleet root for enroll and revoke and says where
// its key came from.
func openMasterRoot(cmd *cobra.Command, dir, passFile string) (pki.Root, error) {
	root, src, err := pki.OpenRoot(dir, rootKeychain, lazyPassphrase(cmd, passFile))
	if err != nil {
		return pki.Root{}, err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "# fleet root key from the %s\n", custodyLabel(src, dir))
	return root, nil
}

func custodyLabel(src pki.RootSource, dir string) string {
	if src == pki.RootFromKeychain {
		return "OS keychain"
	}
	return "passphrase-encrypted file " + filepath.Join(dir, pki.RootKeyFile)
}

func identityInitMasterCmd() *cobra.Command {
	var dir, cn, passFile string
	var validity, crlValidity time.Duration
	c := &cobra.Command{
		Use:   "init-master",
		Short: "Create the fleet root; its key goes into the OS keychain (or a passphrase-encrypted file without one)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cn == "" {
				return errors.New("--cn is required")
			}
			dir, err := resolveMasterDir(dir)
			if err != nil {
				return err
			}
			root, src, err := pki.CreateRoot(dir, cn, validity, rootKeychain, lazyPassphrase(cmd, passFile))
			if err != nil {
				return err
			}
			// An empty CRL, Number 1, so the first bundle carries one: an
			// instance without a CRL refuses every peer (fail closed).
			if _, err := pki.IssueCRL(dir, root, nil, big.NewInt(1), time.Now().Add(crlValidity)); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "# fleet root created in %s; its key is in the %s\n", dir, custodyLabel(src, dir))
			fmt.Fprintln(out, "# back the key up with `knomit identity export-root --keep --out <file>`; losing it ends the fleet")
			// The value each instance confirms with `identity install --root`.
			if id, err := pki.RootID(root.Cert); err == nil {
				fmt.Fprintf(out, "# fleet root fingerprint: %s (give it to each instance for `knomit identity install --root`)\n", id)
			}
			_, err = out.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Cert.Raw}))
			return err
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "master directory to create (root.crt, crl.pem, the ledgers; default <home>/master)")
	c.Flags().StringVar(&cn, "cn", "", "root common name, e.g. knomit-master-<you>")
	c.Flags().StringVar(&passFile, "passphrase-file", "", "file holding the root passphrase (- = stdin); used only when the OS keychain is unavailable")
	c.Flags().DurationVar(&validity, "validity", 10*365*24*time.Hour, "root certificate validity")
	c.Flags().DurationVar(&crlValidity, "crl-validity", crlValidityDefault, "NextUpdate of the initial CRL")
	return c
}

// parseInstancePubkey reads an authorized_keys line (the instance's
// id_ed25519.pub, or the line `knomit serve` prints at first start) from a
// file path or inline, and returns the key and the host from its
// "knomit@<host>" comment, if any.
func parseInstancePubkey(arg string) (ed25519.PublicKey, string, error) {
	line := []byte(arg)
	if b, err := os.ReadFile(arg); err == nil {
		line = b
	}
	pk, comment, _, _, err := ssh.ParseAuthorizedKey(line)
	if err != nil {
		return nil, "", fmt.Errorf("--pubkey: not an authorized_keys line or a file holding one: %w", err)
	}
	cpk, ok := pk.(ssh.CryptoPublicKey)
	if !ok {
		return nil, "", errors.New("--pubkey: unsupported key")
	}
	pub, ok := cpk.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return nil, "", fmt.Errorf("--pubkey: %s key, want ssh-ed25519", pk.Type())
	}
	host, _ := strings.CutPrefix(comment, "knomit@")
	if host == comment {
		host = ""
	}
	return pub, host, nil
}

func identityEnrollCmd() *cobra.Command {
	var dir, pubArg, host, role, passFile, outPath string
	var validity time.Duration
	c := &cobra.Command{
		Use:   "enroll",
		Short: "Sign an instance's EXISTING public key into a certificate bundle (master side)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if pubArg == "" {
				return errors.New("--pubkey is required")
			}
			dir, err := resolveMasterDir(dir)
			if err != nil {
				return err
			}
			pub, commentHost, err := parseInstancePubkey(pubArg)
			if err != nil {
				return err
			}
			if host == "" {
				host = commentHost
			}
			if host == "" {
				return errors.New("--host is required (the public key carries no knomit@<host> comment)")
			}
			root, err := openMasterRoot(cmd, dir, passFile)
			if err != nil {
				return err
			}
			certPEM, rec, err := pki.IssueInstance(dir, root, pub, host, pki.Role(role), validity)
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
			fmt.Fprintf(cmd.ErrOrStderr(), "enrolled %s serial=%s principal=%s@cert not_after=%s\n",
				rec.SAN, rec.Serial, pki.PrincipalKind(pki.Role(role))+":"+rec.Fingerprint, rec.NotAfter.Format(time.RFC3339))
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "master directory (default <home>/master)")
	c.Flags().StringVar(&pubArg, "pubkey", "", "the instance's public key: a file (id_ed25519.pub) or the line itself")
	c.Flags().StringVar(&host, "host", "", "host label for the SAN (default: from the key's knomit@<host> comment)")
	c.Flags().StringVar(&role, "role", string(pki.RoleInstance), "instance or operator")
	c.Flags().StringVar(&passFile, "passphrase-file", "", "file holding the root passphrase (- = stdin)")
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

  - the operator: ` + "`knomit identity init-master`" + ` prints it on its
    "# fleet root fingerprint:" line;
  - the operator, later: ` + "`knomit identity root-fingerprint`" + ` prints it
    from the master directory's root.crt (no key or passphrase needed,
    wherever the key is kept);
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
	fmt.Fprintf(out, "installed in %s\nprincipal: %s:%s@cert\nsan: %s\nnot_after: %s\n",
		dir, pki.PrincipalKind(id.Role), id.Fingerprint, pki.SAN(id.Role, id.Host, id.Fingerprint), id.NotAfter.Format(time.RFC3339))
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
	"has it from `knomit identity init-master` (its `# fleet root fingerprint:` line) or can print it again with " +
	"`knomit identity root-fingerprint` (read from the master directory's root.crt); on an " +
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
	var dir, serialHex, fp, reason, passFile string
	var crlValidity time.Duration
	c := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke a certificate by serial or by fingerprint and reissue the CRL (master side)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (serialHex == "") == (fp == "") {
				return errors.New("exactly one of --serial or --fingerprint is required")
			}
			dir, err := resolveMasterDir(dir)
			if err != nil {
				return err
			}
			serials, err := serialsToRevoke(dir, serialHex, fp)
			if err != nil {
				return err
			}
			root, err := openMasterRoot(cmd, dir, passFile)
			if err != nil {
				return err
			}
			var num *big.Int
			for _, s := range serials {
				if num, err = pki.Revoke(dir, root, pki.Revoked{Serial: s, At: time.Now().UTC(), Reason: reason}, time.Now().Add(crlValidity)); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "revoked serial %s\n", s.Text(16))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "CRL %s is now Number %s; copy it to every instance's [tls].dir\n", filepath.Join(dir, pki.CRLFile), num)
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "master directory (default <home>/master)")
	c.Flags().StringVar(&serialHex, "serial", "", "serial to revoke (hex, as enroll printed it)")
	c.Flags().StringVar(&fp, "fingerprint", "", "revoke EVERY certificate issued for this 64-hex fingerprint")
	c.Flags().StringVar(&reason, "reason", "", "free text recorded in revoked.jsonl")
	c.Flags().StringVar(&passFile, "passphrase-file", "", "file holding the root passphrase (- = stdin)")
	c.Flags().DurationVar(&crlValidity, "crl-validity", crlValidityDefault, "NextUpdate of the reissued CRL")
	return c
}

// serialsToRevoke resolves --serial or --fingerprint against issued.jsonl.
// A serial the master never issued is refused: a typo must not quietly
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
		Short: "Show this instance's fingerprint, certificate and CRL state",
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
	fmt.Fprintf(out, "key: %s\nfingerprint: %s\nshort: %s\n", st.KeyPath, st.Fingerprint, pki.Short(st.Fingerprint))
	switch {
	case !st.Enrolled:
		fmt.Fprintf(out, "certificate: none (not enrolled; see `knomit identity install`)\n")
	case st.IdentityErr != nil:
		fmt.Fprintf(out, "certificate: unreadable identity: %v\n", st.IdentityErr)
	case st.Cert != nil:
		id := st.Cert
		fmt.Fprintf(out, "principal: %s:%s@cert\nsan: %s\nserial: %s\nnot_after: %s\n",
			pki.PrincipalKind(id.Role), id.Fingerprint, pki.SAN(id.Role, id.Host, id.Fingerprint), id.Serial.Text(16), id.NotAfter.Format(time.RFC3339))
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

// stdinIsTerminal is whether the command runs on a terminal. A variable so
// tests can stand in for one; export-root refuses to run without it.
var stdinIsTerminal = func(cmd *cobra.Command) bool { return isTerminal(cmd.InOrStdin()) }

func identityExportRootCmd() *cobra.Command {
	var dir, outPath, passFile string
	var keep bool
	c := &cobra.Command{
		Use:   "export-root",
		Short: "Move the fleet root key into a passphrase-encrypted file (backup or master takeover)",
		Long: `Write the fleet root key, passphrase-encrypted (the same OpenSSH format as
root.key), followed by the root certificate, to a NEW file given by --out.

By default this MOVES the key: once the file has been written and read back,
the OS keychain item and any root.key in the master directory are removed, so
two masters never coexist by accident. --keep leaves the local copy in place
(a backup). The command runs only on a terminal; there is no API or UI for it.
Load the file on the new master with ` + "`knomit identity import-root <file>`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if outPath == "" {
				return errors.New("--out is required: the root key is only ever written to a file")
			}
			if !stdinIsTerminal(cmd) {
				return errors.New("export-root runs only on a terminal: it moves the fleet root key and is never scripted")
			}
			dir, err := resolveMasterDir(dir)
			if err != nil {
				return err
			}
			pass, err := readExportPassphrase(cmd, passFile)
			if err != nil {
				return err
			}
			res, err := pki.ExportRoot(dir, outPath, rootKeychain, pass, keep)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "exported the fleet root key (from the %s) to %s\n", custodyLabel(res.From, dir), outPath)
			switch {
			case keep:
				fmt.Fprintln(out, "kept the local copy (--keep)")
			case res.KeychainErr != nil:
				// The key was read from the file because the keychain could
				// not be read; deleting its item failed too, so it may still
				// be there. Never claim this machine cannot sign.
				if res.RemovedFile {
					fmt.Fprintf(out, "removed %s\n", filepath.Join(dir, pki.RootKeyFile))
				}
				fmt.Fprintf(out, "WARNING: the OS keychain could not delete its item %s/%s (%v): it may STILL hold the fleet root key, so this machine may still sign for the fleet; remove that item by hand\n",
					pki.KeychainService, rootAccount(dir), res.KeychainErr)
			case res.RemovedKeychain || res.RemovedFile:
				fmt.Fprintln(out, "removed the local copy: this machine can no longer sign for the fleet until the file is imported again")
			}
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "master directory (default <home>/master)")
	c.Flags().StringVar(&outPath, "out", "", "file to write (must not exist)")
	c.Flags().StringVar(&passFile, "passphrase-file", "", "file holding the passphrase that encrypts the export, and opens root.key if the key is held there (- = type it on the terminal); it is always confirmed by typing it again")
	c.Flags().BoolVar(&keep, "keep", false, "keep the local copy (a backup) instead of moving the key")
	return c
}

func identityImportRootCmd() *cobra.Command {
	var dir, passFile string
	c := &cobra.Command{
		Use:   "import-root <file>",
		Short: "Load a fleet root key written by export-root into the OS keychain (or root.key without one)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := resolveMasterDir(dir)
			if err != nil {
				return err
			}
			pass, err := readPassphrase(cmd, passFile)
			if err != nil {
				return err
			}
			root, src, err := pki.ImportRoot(dir, args[0], rootKeychain, pass)
			if err != nil {
				return err
			}
			id, _ := pki.RootID(root.Cert)
			fmt.Fprintf(cmd.OutOrStdout(), "imported fleet root %s (%q) into the %s; master directory %s\n",
				id, root.Cert.Subject.CommonName, custodyLabel(src, dir), dir)
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "master directory (default <home>/master)")
	c.Flags().StringVar(&passFile, "passphrase-file", "", "file holding the export's passphrase (- = stdin)")
	return c
}

// readExportPassphrase reads the passphrase that will encrypt an export and
// makes the operator type it (again) on the terminal: after a move the
// export is the only copy of the root key, and a passphrase nobody can
// repeat makes it unrecoverable. With --passphrase-file - both entries are
// typed; with a file, its content must be typed once to confirm. A
// mismatch aborts before anything is written or removed.
func readExportPassphrase(cmd *cobra.Command, path string) ([]byte, error) {
	if path == "" {
		return nil, errors.New("--passphrase-file is required (use - to type it)")
	}
	in := bufio.NewReader(cmd.InOrStdin())
	errOut := cmd.ErrOrStderr()
	readLine := func(prompt string) ([]byte, error) {
		fmt.Fprint(errOut, prompt)
		line, err := in.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		return []byte(strings.TrimRight(line, "\r\n")), nil
	}
	var first []byte
	if path == "-" {
		p, err := readLine("Passphrase for the export file: ")
		if err != nil {
			return nil, err
		}
		first = p
	} else {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read passphrase: %w", err)
		}
		first = bytes.TrimRight(raw, "\r\n")
	}
	if len(first) == 0 {
		return nil, errors.New("the passphrase is empty")
	}
	again, err := readLine("Type the export passphrase again to confirm: ")
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(first, again) {
		return nil, errors.New("the passphrase was not confirmed (the second entry differs); nothing was written or removed")
	}
	return first, nil
}

// rootAccount is the keychain account of the root in dir, for messages.
func rootAccount(dir string) string {
	c, err := pki.LoadRootCert(filepath.Join(dir, pki.RootCertFile))
	if err != nil {
		return pki.KeychainAccount("<root id>")
	}
	id, err := pki.RootID(c)
	if err != nil {
		return pki.KeychainAccount("<root id>")
	}
	return pki.KeychainAccount(id)
}

func identityRootFingerprintCmd() *cobra.Command {
	var dir string
	c := &cobra.Command{
		Use:   "root-fingerprint",
		Short: "Print the fleet root fingerprint (the value `install --root` takes) from the master directory's root.crt",
		Long: `Print the fleet root fingerprint: 64 hex characters, the SHA-256 of the
root public key's SSH wire encoding, read from <dir>/root.crt. It needs
neither the root key nor its passphrase, so it works wherever the key is kept
(OS keychain or root.key). Give the value to each instance for
` + "`knomit identity install --root`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := resolveMasterDir(dir)
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
	c.Flags().StringVar(&dir, "dir", "", "master directory holding root.crt (default <home>/master)")
	return c
}
