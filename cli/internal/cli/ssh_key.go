package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func (a *App) newSSHKeyCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ssh-key", Aliases: []string{"ssh-keys"}, Short: "Manage project-scoped SSH keys"}
	cmd.AddCommand(a.newSSHKeyListCommand())
	cmd.AddCommand(a.newSSHKeyAddCommand())
	cmd.AddCommand(a.newSSHKeyDeleteCommand())
	return cmd
}

func (a *App) newSSHKeyListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List SSH keys", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		res, err := client.ListSSHKeys(cmd.Context(), apiclientgen.ListSSHKeysParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListSSHKeysBody](res)
		if err != nil {
			return err
		}
		return a.writeSSHKeys(cmd, body.GetSshKeys())
	}}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newSSHKeyAddCommand() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "add [PUBKEY_FILE|-]",
		Short: "Enroll an SSH public key for this project",
		Long: "Enroll an SSH public key for this project. With a file argument (or \"-\" for\n" +
			"stdin), that key is enrolled directly. With no argument, keys from a running\n" +
			"SSH_AUTH_SOCK agent (falling back to ~/.ssh/*.pub) are offered for enrollment;\n" +
			"listing an agent's public keys is a convenience only and proves nothing about\n" +
			"possession of the private half.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			publicKeyLine, err := resolveSSHKeyToEnroll(cmd, args)
			if err != nil {
				return err
			}
			body := &apimodel.CreateSSHKeyBody{PublicKey: publicKeyLine}
			if strings.TrimSpace(name) != "" {
				body.SetName(apiclientgen.NewOptString(strings.TrimSpace(name)))
			}
			res, err := client.CreateSSHKey(cmd.Context(), body, apiclientgen.CreateSSHKeyParams{ProjectId: projectID})
			if err != nil {
				return err
			}
			key, err := expectResponse[apimodel.SSHKey](res)
			if err != nil {
				return err
			}
			return a.writeSSHKey(cmd, key)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Optional label for the key")
	return cmd
}

func (a *App) newSSHKeyDeleteCommand() *cobra.Command {
	return &cobra.Command{Use: "rm SSH_KEY_ID...", Aliases: []string{"delete"}, Short: "Revoke SSH keys", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		return runActionMany(cmd, args, "SSH key", "deleted", func(arg string) (string, error) {
			keyID, err := a.resolveSSHKeyID(cmd.Context(), client, projectID, arg)
			if err != nil {
				return "", err
			}
			res, err := client.DeleteSSHKey(cmd.Context(), apiclientgen.DeleteSSHKeyParams{ProjectId: projectID, SshKeyId: keyID})
			if err != nil {
				return "", err
			}
			if err := expectNoContent[apiclientgen.DeleteSSHKeyNoContent](res); err != nil {
				return "", err
			}
			return keyID, nil
		})
	}}
}

// resolveSSHKeyToEnroll returns the raw authorized_keys(5) line to enroll: the
// explicit file/stdin argument when given, otherwise one key chosen from the
// running SSH agent (falling back to ~/.ssh/*.pub).
func resolveSSHKeyToEnroll(cmd *cobra.Command, args []string) (string, error) {
	if len(args) == 1 {
		return readPublicKeyArg(cmd, args[0])
	}
	candidates, err := discoverLocalPublicKeys(cmd.Context())
	if err != nil {
		return "", err
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no local public keys found; pass a public key file, or enroll one via SSH_AUTH_SOCK / ~/.ssh")
	}
	items := make([]pickerItem, 0, len(candidates))
	for _, c := range candidates {
		items = append(items, pickerItem{id: c.line, title: c.fingerprint, detail: c.comment})
	}
	picked, err := pickOne(cmd, "Which key should be enrolled?", items, pickerOptions{
		empty:     "no local public keys found",
		ambiguous: "multiple local public keys found (" + strings.Join(fingerprintList(candidates), ", ") + "); pass one explicitly as a file argument",
	})
	if err != nil {
		return "", err
	}
	return picked, nil
}

func readPublicKeyArg(cmd *cobra.Command, arg string) (string, error) {
	if arg == "-" {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("read public key from stdin: %w", err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	data, err := os.ReadFile(arg)
	if err != nil {
		return "", fmt.Errorf("read public key file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

type discoveredPublicKey struct {
	line        string
	fingerprint string
	comment     string
}

func fingerprintList(keys []discoveredPublicKey) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.fingerprint)
	}
	return out
}

// discoverLocalPublicKeys lists public keys from a running SSH_AUTH_SOCK
// agent, falling back to ~/.ssh/*.pub when no agent is reachable. This is
// enrollment convenience only: listing an agent's public keys proves nothing
// about possession of the private half, and no code path may treat agent
// presence as evidence of anything (ADR 0024 §6) — the actual authorization
// is the authenticated CreateSSHKey call that follows.
func discoverLocalPublicKeys(ctx context.Context) ([]discoveredPublicKey, error) {
	if keys, err := discoverAgentPublicKeys(ctx); err == nil && len(keys) > 0 {
		return keys, nil
	}
	return discoverSSHDirPublicKeys()
}

func discoverAgentPublicKeys(ctx context.Context) ([]discoveredPublicKey, error) {
	sock := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	if sock == "" {
		return nil, fmt.Errorf("SSH_AUTH_SOCK is not set")
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, fmt.Errorf("dial SSH agent: %w", err)
	}
	defer conn.Close()
	identities, err := agent.NewClient(conn).List()
	if err != nil {
		return nil, fmt.Errorf("list SSH agent identities: %w", err)
	}
	out := make([]discoveredPublicKey, 0, len(identities))
	for _, id := range identities {
		pub, err := ssh.ParsePublicKey(id.Marshal())
		if err != nil {
			continue
		}
		out = append(out, discoveredPublicKey{
			line:        strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + id.Comment,
			fingerprint: ssh.FingerprintSHA256(pub),
			comment:     id.Comment,
		})
	}
	return out, nil
}

func discoverSSHDirPublicKeys() ([]discoveredPublicKey, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(home, ".ssh", "*.pub"))
	if err != nil {
		return nil, err
	}
	out := make([]discoveredPublicKey, 0, len(matches))
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		pub, comment, _, _, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			continue
		}
		out = append(out, discoveredPublicKey{
			line:        strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " " + comment,
			fingerprint: ssh.FingerprintSHA256(pub),
			comment:     comment,
		})
	}
	return out, nil
}

func (a *App) resolveSSHKeyID(ctx context.Context, client *apiclientgen.Client, projectID, value string) (string, error) {
	id, err := parseIDArg(value, "SSH key ID")
	if err != nil || !isResolvableShortID(id) {
		return id, err
	}
	res, err := client.ListSSHKeys(ctx, apiclientgen.ListSSHKeysParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.ListSSHKeysBody](res)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(body.GetSshKeys()))
	for _, key := range body.GetSshKeys() {
		ids = append(ids, key.ID)
	}
	return resolveShortID(id, "SSH key ID", ids)
}
