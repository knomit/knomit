package identity

import (
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	sshsigMagic     = "SSHSIG"
	sshsigVersion   = 1
	sshsigNamespace = "git"
	sshsigHashAlgo  = "sha512"
)

// appendSSHString appends an SSH wire-format string (uint32 BE length + data).
func appendSSHString(buf, data []byte) []byte {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(data)))
	buf = append(buf, lenBuf[:]...)
	buf = append(buf, data...)
	return buf
}

// SignCommit signs a commit payload using the SSHSIG format.
// Returns the armored signature string for the commit's PGPSignature field.
func SignCommit(signer ssh.Signer, payload []byte) (string, error) {
	// Build the "signed data" blob.
	h := sha512.Sum512(payload)

	var signed []byte
	signed = append(signed, sshsigMagic...)                  // magic, raw, NOT length-prefixed
	signed = appendSSHString(signed, []byte(sshsigNamespace))
	signed = appendSSHString(signed, nil)                    // reserved
	signed = appendSSHString(signed, []byte(sshsigHashAlgo))
	signed = appendSSHString(signed, h[:])

	sig, err := signer.Sign(rand.Reader, signed)
	if err != nil {
		return "", fmt.Errorf("sshsig: sign: %w", err)
	}

	// Marshal the raw SSH signature blob (algorithm string + signature bytes).
	sigBlob := ssh.Marshal(sig)

	// Build the envelope.
	pubBlob := signer.PublicKey().Marshal()

	var env []byte
	env = append(env, sshsigMagic...)               // magic, raw
	var verBuf [4]byte
	binary.BigEndian.PutUint32(verBuf[:], sshsigVersion)
	env = append(env, verBuf[:]...)                  // version
	env = appendSSHString(env, pubBlob)              // public key
	env = appendSSHString(env, []byte(sshsigNamespace))
	env = appendSSHString(env, nil)                  // reserved
	env = appendSSHString(env, []byte(sshsigHashAlgo))
	env = appendSSHString(env, sigBlob)              // signature blob

	return armor(env), nil
}

// armor wraps raw bytes in PEM-like SSH signature armor with 70-char lines.
func armor(data []byte) string {
	encoded := base64.StdEncoding.EncodeToString(data)

	var sb strings.Builder
	sb.WriteString("-----BEGIN SSH SIGNATURE-----\n")
	for len(encoded) > 0 {
		end := 70
		if end > len(encoded) {
			end = len(encoded)
		}
		sb.WriteString(encoded[:end])
		sb.WriteByte('\n')
		encoded = encoded[end:]
	}
	sb.WriteString("-----END SSH SIGNATURE-----")
	return sb.String()
}
