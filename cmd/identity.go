package cmd

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
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
		// A refusal (wrong key, older CRL, unknown serial) is an answer, not a
		// usage mistake: print the error alone.
		SilenceUsage: true,
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
				rec.SAN, rec.Serial, principalKind(pki.Role(role))+":"+rec.Fingerprint, rec.NotAfter.Format(time.RFC3339))
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

func principalKind(r pki.Role) string {
	if r == pki.RoleOperator {
		return "operator"
	}
	return "instance"
}

// splitBundle returns the instance certificate, the root certificate and the
// CRL from a bundle, refusing anything else in it.
func splitBundle(raw []byte) (leaf, root *x509.Certificate, rootPEM, crlPEM []byte, crl *x509.RevocationList, err error) {
	for rest := raw; ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		switch blk.Type {
		case "CERTIFICATE":
			c, perr := x509.ParseCertificate(blk.Bytes)
			if perr != nil {
				return nil, nil, nil, nil, nil, perr
			}
			if c.IsCA {
				if root != nil {
					return nil, nil, nil, nil, nil, errors.New("bundle holds two root certificates")
				}
				root, rootPEM = c, pem.EncodeToMemory(blk)
			} else {
				if leaf != nil {
					return nil, nil, nil, nil, nil, errors.New("bundle holds two instance certificates")
				}
				leaf = c
			}
		case "X509 CRL":
			if crl != nil {
				return nil, nil, nil, nil, nil, errors.New("bundle holds two CRLs")
			}
			crlPEM = pem.EncodeToMemory(blk)
			if crl, err = x509.ParseRevocationList(blk.Bytes); err != nil {
				return nil, nil, nil, nil, nil, fmt.Errorf("%w: %v", pki.ErrCRLInvalid, err)
			}
		default:
			return nil, nil, nil, nil, nil, fmt.Errorf("bundle holds an unexpected %q block", blk.Type)
		}
	}
	if leaf == nil || root == nil || crl == nil {
		return nil, nil, nil, nil, nil, errors.New("bundle must hold an instance certificate, a root certificate and a CRL")
	}
	return leaf, root, rootPEM, crlPEM, crl, nil
}

func identityInstallCmd() *cobra.Command {
	var bundlePath string
	var replaceRoot bool
	c := &cobra.Command{
		Use:   "install",
		Short: "Install an enrollment bundle on THIS instance (instance side)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			var raw []byte
			if bundlePath == "" || bundlePath == "-" {
				raw, err = io.ReadAll(cmd.InOrStdin())
			} else {
				raw, err = os.ReadFile(bundlePath)
			}
			if err != nil {
				return err
			}
			return installBundle(cmd.OutOrStdout(), cfg, raw, replaceRoot)
		},
	}
	c.Flags().StringVar(&bundlePath, "bundle", "", "bundle file from `knomit identity enroll` (default/-: stdin)")
	c.Flags().BoolVar(&replaceRoot, "replace-root", false, "allow a bundle from a DIFFERENT fleet root (moves this instance to another fleet)")
	return c
}

// installBundle is `identity install` without the flag parsing, so tests can
// drive it. Every check runs BEFORE anything is written.
func installBundle(out io.Writer, cfg config.Config, raw []byte, replaceRoot bool) error {
	leaf, root, rootPEM, crlPEM, crl, err := splitBundle(raw)
	if err != nil {
		return err
	}
	keyPath := app.ResolveKeyPath(cfg)
	_, pub, err := pki.LoadSigner(keyPath)
	if err != nil {
		return fmt.Errorf("instance key: %w", err)
	}
	if !pub.Equal(leaf.PublicKey) {
		return fmt.Errorf("the bundle's certificate is for key %s, not this instance's %s (%s)",
			pki.Short(fingerprintOf(leaf)), pki.Short(pki.Fingerprint(pub)), keyPath)
	}
	id, err := pki.VerifyInstanceChain(leaf, nil, root, crl, time.Now(), pki.UsageClient)
	if err != nil {
		return err
	}
	dir := cfg.TLS.Dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	// A different root is a different fleet: refuse unless asked. Nothing is
	// reset when it is asked: the CRL watermark is kept PER ROOT
	// (pki.AcceptedNumber), so the new fleet's CRL #1 is judged against the
	// new root's entry, while an old bundle of a root seen before — after a
	// detour A -> B -> A — is still judged against that root's own entry and
	// refused if older.
	if cur, err := pki.LoadRootCert(filepath.Join(dir, pki.RootCertFile)); err == nil && !cur.Equal(root) && !replaceRoot {
		return fmt.Errorf("this instance is enrolled under root %q and the bundle is from a different root %q; pass --replace-root to move it to the other fleet",
			cur.Subject.CommonName, root.Subject.CommonName)
	}
	last, err := pki.AcceptedNumber(dir, root) // the BUNDLE's root, not the installed one
	if err != nil {
		return err
	}
	if _, err := pki.CheckCRL(crl, root, last); err != nil {
		return fmt.Errorf("the bundle's CRL is older than the one this instance already holds: %w", err)
	}
	for _, f := range []struct {
		name string
		data []byte
	}{
		// The server's reloader adopts the three only as a set that verifies
		// together, so the rename order cannot expose a torn state.
		{pki.RootCertFile, rootPEM},
		{pki.CRLFile, crlPEM},
		{pki.InstanceCertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})},
	} {
		if err := writeAtomic(filepath.Join(dir, f.name), f.data, 0o600); err != nil {
			return err
		}
	}
	if err := pki.RecordAcceptedNumber(dir, root, crl.Number); err != nil {
		return err
	}
	fmt.Fprintf(out, "installed in %s\nprincipal: %s:%s@cert\nsan: %s\nnot_after: %s\n",
		dir, principalKind(id.Role), id.Fingerprint, pki.SAN(id.Role, id.Host, id.Fingerprint), id.NotAfter.Format(time.RFC3339))
	if cfg.TLS.Addr == "" {
		fmt.Fprintln(out, "the TLS listener is off; to accept enrolled peers add to knomit.toml:\n  [tls]\n  addr = \"0.0.0.0:19279\"")
	} else {
		fmt.Fprintln(out, "a running `knomit serve` picks this up within its recheck interval or on its next handshake; a stopped one on start")
	}
	return nil
}

func fingerprintOf(c *x509.Certificate) string {
	if pub, ok := c.PublicKey.(ed25519.PublicKey); ok && len(pub) == ed25519.PublicKeySize {
		return pki.Fingerprint(pub)
	}
	return "????????"
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
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
	keyPath := app.ResolveKeyPath(cfg)
	_, pub, err := pki.LoadSigner(keyPath)
	if err != nil {
		return fmt.Errorf("instance key: %w", err)
	}
	fp := pki.Fingerprint(pub)
	fmt.Fprintf(out, "key: %s\nfingerprint: %s\nshort: %s\n", keyPath, fp, pki.Short(fp))
	dir := cfg.TLS.Dir
	if !pki.HasInstanceCert(dir) {
		fmt.Fprintf(out, "certificate: none (not enrolled; see `knomit identity install`)\n")
	} else if raw, err := os.ReadFile(filepath.Join(dir, pki.InstanceCertFile)); err == nil {
		if blk, _ := pem.Decode(raw); blk != nil {
			if c, err := x509.ParseCertificate(blk.Bytes); err == nil {
				id, ierr := pki.IdentityOf(c)
				if ierr != nil {
					fmt.Fprintf(out, "certificate: unreadable identity: %v\n", ierr)
				} else {
					fmt.Fprintf(out, "principal: %s:%s@cert\nsan: %s\nserial: %s\nnot_after: %s\n",
						principalKind(id.Role), id.Fingerprint, pki.SAN(id.Role, id.Host, id.Fingerprint), c.SerialNumber.Text(16), c.NotAfter.Format(time.RFC3339))
				}
			}
		}
	}
	if crl, err := pki.LoadCRL(filepath.Join(dir, pki.CRLFile)); err == nil {
		fmt.Fprintf(out, "crl_number: %s\ncrl_next_update: %s\n", crl.Number, crl.NextUpdate.Format(time.RFC3339))
	}
	if cfg.TLS.Addr == "" {
		fmt.Fprintln(out, "tls_listener: off ([tls].addr is empty)")
	} else {
		fmt.Fprintf(out, "tls_listener: %s\n", cfg.TLS.Addr)
	}
	return nil
}
