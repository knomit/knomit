package vcs

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
	SSHSigMagic     = "SSHSIG"
	SSHSigVersion   = 1
	SSHSigNamespace = "git"
	SSHSigHashAlgo  = "sha512"
)

// AppendSSHString appends an SSH wire-format string (uint32 BE length + data).
func AppendSSHString(buf, data []byte) []byte {
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
	signed = append(signed, SSHSigMagic...) // magic, raw, NOT length-prefixed
	signed = AppendSSHString(signed, []byte(SSHSigNamespace))
	signed = AppendSSHString(signed, nil) // reserved
	signed = AppendSSHString(signed, []byte(SSHSigHashAlgo))
	signed = AppendSSHString(signed, h[:])

	sig, err := signer.Sign(rand.Reader, signed)
	if err != nil {
		return "", fmt.Errorf("sshsig: sign: %w", err)
	}

	// Marshal the raw SSH signature blob (algorithm string + signature bytes).
	sigBlob := ssh.Marshal(sig)

	// Build the envelope.
	pubBlob := signer.PublicKey().Marshal()

	var env []byte
	env = append(env, SSHSigMagic...) // magic, raw
	var verBuf [4]byte
	binary.BigEndian.PutUint32(verBuf[:], SSHSigVersion)
	env = append(env, verBuf[:]...)     // version
	env = AppendSSHString(env, pubBlob) // public key
	env = AppendSSHString(env, []byte(SSHSigNamespace))
	env = AppendSSHString(env, nil) // reserved
	env = AppendSSHString(env, []byte(SSHSigHashAlgo))
	env = AppendSSHString(env, sigBlob) // signature blob

	return Armor(env), nil
}

// armor wraps raw bytes in PEM-like SSH signature armor with 70-char lines.
func Armor(data []byte) string {
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
