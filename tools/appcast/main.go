// knomit-appcast signs desktop release artifacts and publishes the Sparkle
// appcast feed the in-app updater reads.
//
//	appcast keygen                       # once, by hand, at setup
//	appcast sign  <file>...              # writes <file>.ed25519 beside each
//	appcast feed  -releases <json> ...   # emits appcast.xml
//
// Signatures cover the SHA-256 digest of each artifact, which is what
// wails/v3 pkg/updater verifies. They are NOT interchangeable with signatures
// from Sparkle's own sign_update, which signs file contents — the feed
// vocabulary is shared, the signing scheme is not.
//
// The keypair is generated once by a human and never by CI. The private half
// signs releases; the public half is baked into every desktop binary via
// -ldflags. Because clients pin the public key at build time, a lost private
// key cannot be rotated for anyone already running — see runKeygen.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"knomit/internal/version"
)

const usage = `knomit appcast — sign release artifacts and build the update feed

usage:
  appcast keygen
  appcast sign <file>...
  appcast feed -releases <path> -repo <owner/repo> -link <feed-url> [-out <path>]
  appcast version

sign reads the base64 Ed25519 private key from $UPDATE_PRIVATE_KEY and writes
<file>.ed25519 next to each input.

feed reads the GitHub releases API JSON from -releases and emits appcast.xml.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "keygen":
		err = runKeygen()
	case "sign":
		err = runSign(os.Args[2:])
	case "feed":
		err = runFeed(os.Args[2:])
	case "version":
		fmt.Println(version.String())
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "appcast:", err)
		os.Exit(1)
	}
}

// runKeygen prints a fresh keypair. Run once, by hand, off CI: the private key
// is the only thing that can ever reach installed clients, and it cannot be
// rotated for them — every shipped binary pins the matching public half.
func runKeygen() error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	fmt.Println("public  (repo variable UPDATE_PUBLIC_KEY):")
	fmt.Println(" ", base64.StdEncoding.EncodeToString(pub))
	fmt.Println("private (repo secret   UPDATE_PRIVATE_KEY):")
	fmt.Println(" ", base64.StdEncoding.EncodeToString(priv))
	fmt.Println()
	fmt.Println("Back up the private key offline. Losing it ends the update")
	fmt.Println("channel for every already-installed client — a new keypair")
	fmt.Println("cannot reach them, they must reinstall by hand.")
	return nil
}

func runSign(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("sign needs at least one file")
	}
	priv, err := PrivateKeyFromBase64(os.Getenv("UPDATE_PRIVATE_KEY"))
	if err != nil {
		return fmt.Errorf("$UPDATE_PRIVATE_KEY: %w", err)
	}
	signed := 0
	for _, path := range args {
		// The release workflow passes a glob over the whole staging dir, so a
		// re-run would otherwise sign the sidecars it produced the first time
		// and leave <artifact>.ed25519.ed25519 behind.
		if strings.HasSuffix(path, sigSuffix) {
			continue
		}
		sig, serr := SignFile(priv, path)
		if serr != nil {
			return serr
		}
		out := path + sigSuffix
		if werr := os.WriteFile(out, []byte(sig), 0o644); werr != nil {
			return werr
		}
		fmt.Println("signed", out)
		signed++
	}
	if signed == 0 {
		return fmt.Errorf("no signable files in %d argument(s) — all were signatures", len(args))
	}
	return nil
}
