package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// sshIdentityFileName is the key `disco box ssh-config` generates and enrolls
// so that connecting needs no separate key setup. It lives under the CLI's own
// state directory rather than ~/.ssh: discobox generates, enrolls, and
// replaces it, so it belongs with the CLI's other machine-local state instead
// of among the keys the user manages by hand.
const sshIdentityFileName = "id_ed25519"

func defaultSSHIdentityPath() string {
	return filepath.Join(cliStateDir(), "ssh", sshIdentityFileName)
}

// loadOrCreateSSHIdentity returns the authorized_keys(5) public key line for
// the identity at path, generating an ed25519 keypair there when it is absent.
// It reports whether it generated one, so the caller can say so rather than
// silently creating a credential.
//
// The private key is written in OpenSSH's own format, not the PKCS#8 PEM the
// server uses for its host key: the file here is read by the `ssh` binary,
// whose ed25519 support historically covers only "OPENSSH PRIVATE KEY", while
// the server's host key is only ever parsed by x/crypto/ssh.
func loadOrCreateSSHIdentity(path string) (publicKeyLine string, created bool, err error) {
	if line, err := readSSHIdentityPublicKey(path); err == nil {
		return line, false, nil
	} else if !os.IsNotExist(err) {
		return "", false, err
	}

	if err := ensureStateDir(filepath.Dir(path)); err != nil {
		return "", false, fmt.Errorf("create SSH identity directory: %w", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", false, fmt.Errorf("generate SSH identity: %w", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, sshIdentityComment())
	if err != nil {
		return "", false, fmt.Errorf("marshal SSH identity: %w", err)
	}
	// O_EXCL so a concurrent ssh-config cannot have its key replaced by this
	// one: whoever loses reads the winner's key rather than overwriting it,
	// which would revoke a key the winner may already have enrolled.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			line, readErr := readSSHIdentityPublicKey(path)
			if readErr != nil {
				return "", false, readErr
			}
			return line, false, nil
		}
		return "", false, fmt.Errorf("write SSH identity: %w", err)
	}
	if _, err := file.Write(pem.EncodeToMemory(block)); err != nil {
		_ = file.Close()
		return "", false, fmt.Errorf("write SSH identity: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", false, fmt.Errorf("write SSH identity: %w", err)
	}
	// The private key is the reason any of this is restricted: ssh refuses to
	// use one another principal can read, and on Windows the 0600 above did not
	// make it so.
	if err := restrictToUser(path); err != nil {
		return "", false, fmt.Errorf("restrict SSH identity to this user: %w", err)
	}

	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", false, fmt.Errorf("derive SSH identity public key: %w", err)
	}
	line := publicKeyLineWithComment(sshPub, sshIdentityComment())
	// 0600, though a public key is public: it sits beside the private key in a
	// directory only this user reads, and a looser mode here would buy nothing.
	if err := os.WriteFile(path+".pub", []byte(line+"\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("write SSH identity public key: %w", err)
	}
	return line, true, nil
}

// readSSHIdentityPublicKey derives the public key line from the private key on
// disk rather than trusting the adjacent .pub file, which a user may have
// edited, truncated, or left behind from a different key.
func readSSHIdentityPublicKey(path string) (string, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	signer, err := ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return "", fmt.Errorf("parse SSH identity %s: %w", path, err)
	}
	return publicKeyLineWithComment(signer.PublicKey(), sshIdentityComment()), nil
}

func publicKeyLineWithComment(key ssh.PublicKey, comment string) string {
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	if comment == "" {
		return line
	}
	return line + " " + comment
}

// sshIdentityComment labels the key with where it came from, so it is
// identifiable in `disco box ssh-key ls` on a project several machines enroll
// into.
func sshIdentityComment() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "disco"
	}
	return "disco@" + host
}
