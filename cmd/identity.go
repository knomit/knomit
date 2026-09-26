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
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"knomit/internal/app"
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
	c.AddCommand(identityInitMasterCmd(), identityEnrollCmd(), identityInstallCmd(), identityRevokeCmd(), identityShowCmd())
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

func identityInitMasterCmd() *cobra.Command {
	var dir, cn, passFile string
	var validity, crlValidity time.Duration
	c := &cobra.Command{
		Use:   "init-master",
		Short: "Create the fleet root (run on the operator's machine, never on a fleet instance)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dir == "" || cn == "" {
				return errors.New("--dir and --cn are required")
			}
			pass, err := readPassphrase(cmd, passFile)
			if err != nil {
				return err
			}
			root, err := pki.NewRoot(dir, cn, pass, validity)
			if err != nil {
				return err
			}
			// An empty CRL, Number 1, so the first bundle carries one: an
			// instance without a CRL refuses every peer (fail closed).
			if _, err := pki.IssueCRL(dir, root, nil, big.NewInt(1), time.Now().Add(crlValidity)); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "# fleet root created in %s (root.key is passphrase-encrypted; keep this directory OFF fleet machines)\n", dir)
			_, err = out.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Cert.Raw}))
			return err
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "master directory to create (root.key, root.crt, crl.pem, issued.jsonl)")
	c.Flags().StringVar(&cn, "cn", "", "root common name, e.g. knomit-master-<you>")
	c.Flags().StringVar(&passFile, "passphrase-file", "", "file holding the root passphrase (- = stdin)")
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
			if dir == "" || pubArg == "" {
				return errors.New("--dir and --pubkey are required")
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
			pass, err := readPassphrase(cmd, passFile)
			if err != nil {
				return err
			}
			root, err := pki.LoadRoot(dir, pass)
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
	c.Flags().StringVar(&dir, "dir", "", "master directory")
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
needs no confirmation.`,
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
			if !fromStdin && isTerminal(cmd.InOrStdin()) {
				opts.prompt = cmd.InOrStdin()
			}
			return installBundle(cmd.OutOrStdout(), cfg, raw, opts)
		},
	}
	c.Flags().StringVar(&bundlePath, "bundle", "", "bundle file from `knomit identity enroll` (default/-: stdin)")
	c.Flags().BoolVar(&opts.replaceRoot, "replace-root", false, "allow a bundle from a DIFFERENT fleet root (moves this instance to another fleet; the new root must still be confirmed)")
	c.Flags().StringVar(&opts.root, "root", "", "the fleet root fingerprint the operator gave you out of band: confirms a bundle whose root matches it")
	c.Flags().BoolVar(&opts.yes, "yes", false, "trust the bundle's fleet root without confirming it")
	return c
}

// installOpts is how `identity install` was asked to confirm a fleet root.
type installOpts struct {
	replaceRoot bool
	root        string    // --root: the out-of-band fingerprint
	yes         bool      // --yes
	prompt      io.Reader // the terminal to ask on; nil when there is none
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
	dir, keyPath := cfg.TLS.Dir, app.ResolveKeyPath(cfg)
	preview, err := pki.PreviewBundle(keyPath, raw)
	if err != nil {
		return err
	}
	st, err := pki.Status(dir, keyPath)
	if err != nil {
		return err
	}
	// An installed root.crt that does not load reads as none here; pki
	// refuses it whatever is expected.
	installed := pki.RootInfo{}
	if st.Root != nil {
		installed = *st.Root
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
		if !strings.EqualFold(strings.TrimSpace(opts.root), p.Root.Fingerprint) {
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
	return fmt.Errorf("the bundle's fleet root %s is not confirmed: compare it with the fingerprint the fleet operator gave you, then rerun with --root %s (or --yes to skip the check); nothing was installed",
		p.Root.Fingerprint, p.Root.Fingerprint)
}

func identityRevokeCmd() *cobra.Command {
	var dir, serialHex, fp, reason, passFile string
	var crlValidity time.Duration
	c := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke a certificate by serial or by fingerprint and reissue the CRL (master side)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if dir == "" || (serialHex == "") == (fp == "") {
				return errors.New("--dir and exactly one of --serial or --fingerprint are required")
			}
			serials, err := serialsToRevoke(dir, serialHex, fp)
			if err != nil {
				return err
			}
			pass, err := readPassphrase(cmd, passFile)
			if err != nil {
				return err
			}
			root, err := pki.LoadRoot(dir, pass)
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
	c.Flags().StringVar(&dir, "dir", "", "master directory")
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
	st, err := pki.Status(cfg.TLS.Dir, app.ResolveKeyPath(cfg))
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
