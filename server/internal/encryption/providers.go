package encryption

import (
	"context"
	"crypto/cipher"
	"errors"
	"fmt"
	"os"
	"strings"

	kmsgcp "cloud.google.com/go/kms/apiv1"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azkeys"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/discobox-ai/envelope/pkg/awskms"
	"github.com/discobox-ai/envelope/pkg/azurekeyvault"
	"github.com/discobox-ai/envelope/pkg/envelope"
	"github.com/discobox-ai/envelope/pkg/gcpkms"
	"github.com/discobox-ai/envelope/pkg/localkey"
	"github.com/discobox-ai/envelope/pkg/vaulttransit"

	"github.com/discobox-ai/discobox/server/internal/config"
)

// New builds the keyring cfg configures over keys, or nil when cfg names no
// provider. Each provider checks that its key exists and can wrap, so a
// misconfigured one fails here, before the server serves anything. ctx
// bounds the keyring's life: what it keeps running for a Vault login (its
// renewal, or a token file followed) stops when ctx ends or at Close.
func New(ctx context.Context, cfg config.EncryptionSettings, keys envelope.KeyStore) (*Keyring, error) {
	if cfg.Provider == "" {
		return nil, nil
	}
	k := &Keyring{}
	var closers []func()
	k.stop = func() {
		for _, c := range closers {
			c()
		}
	}
	build := func(name string) (envelope.Provider, error) {
		p, stop, err := newProvider(ctx, cfg, name)
		if stop != nil {
			closers = append(closers, stop)
		}
		if err != nil {
			return nil, fmt.Errorf("encryption provider %s: %w", name, err)
		}
		return p, nil
	}
	primary, err := build(cfg.Provider)
	if err != nil {
		k.Close()
		return nil, err
	}
	envCfg := envelope.Config{Provider: primary, Store: keys, CacheTTL: cfg.CacheTTL}
	if cfg.Previous != "" {
		previous, err := build(cfg.Previous)
		if err != nil {
			k.Close()
			return nil, err
		}
		envCfg.Unwrappers = []envelope.Provider{previous}
	}
	// The local key opens what an earlier release sealed with encryptionKey
	// whichever provider wraps data keys now, so a server that moves
	// straight to a KMS with only its old key configured re-seals its values.
	if strings.TrimSpace(cfg.Local.Key) != "" {
		if k.legacy, err = legacyKeys(cfg.Local); err != nil {
			k.Close()
			return nil, err
		}
	}
	if k.envelope, err = envelope.New(envCfg); err != nil {
		k.Close()
		return nil, err
	}
	k.primary = primary
	return k, nil
}

func newProvider(ctx context.Context, cfg config.EncryptionSettings, name string) (envelope.Provider, func(), error) {
	switch name {
	case config.EncryptionLocal:
		key, err := decodeLocalKey("encryption.local.key", cfg.Local.Key)
		if err != nil {
			return nil, nil, err
		}
		var retired [][]byte
		for i, raw := range cfg.Local.RetiredKeys {
			old, err := decodeLocalKey(fmt.Sprintf("encryption.local.retiredKeys[%d]", i), raw)
			if err != nil {
				return nil, nil, err
			}
			retired = append(retired, old)
		}
		p, err := localkey.New(key, retired...)
		return p, nil, err
	case config.EncryptionAWSKMS:
		var opts []func(*awsconfig.LoadOptions) error
		if region := awsRegion(cfg.AWSKMS); region != "" {
			opts = append(opts, awsconfig.WithRegion(region))
		}
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return nil, nil, err
		}
		if awsCfg.Region == "" {
			return nil, nil, errors.New("no region: encryption.awskms.key is not an ARN, so set encryption.awskms.region or AWS_REGION")
		}
		p, err := awskms.New(ctx, kms.NewFromConfig(awsCfg), cfg.AWSKMS.Key)
		return p, nil, err
	case config.EncryptionGCPKMS:
		client, err := kmsgcp.NewKeyManagementClient(ctx)
		if err != nil {
			return nil, nil, err
		}
		closeClient := func() { _ = client.Close() }
		p, err := gcpkms.New(ctx, client, cfg.GCPKMS.Key)
		return p, closeClient, err
	case config.EncryptionAzureKeyVault:
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, nil, err
		}
		client, err := azkeys.NewClient(cfg.AzureKeyVault.VaultURL, cred, nil)
		if err != nil {
			return nil, nil, err
		}
		p, err := azurekeyvault.New(ctx, client, cfg.AzureKeyVault.Key)
		return p, nil, err
	case config.EncryptionVaultTransit:
		client, stop, err := newVaultClient(ctx, cfg.VaultTransit)
		if err != nil {
			return nil, stop, err
		}
		p, err := vaulttransit.New(ctx, client, vaulttransit.Config{Mount: cfg.VaultTransit.Mount, Key: cfg.VaultTransit.Key})
		return p, stop, err
	default:
		return nil, nil, fmt.Errorf("unknown provider %q", name)
	}
}

// awsRegion is the configured region, or the one in the key's ARN.
func awsRegion(cfg config.AWSKMSEncryptionSettings) string {
	if region := strings.TrimSpace(cfg.Region); region != "" {
		return region
	}
	// arn:partition:kms:region:account:key/... or alias/...
	if parts := strings.SplitN(cfg.Key, ":", 6); len(parts) == 6 && parts[0] == "arn" {
		return parts[3]
	}
	return ""
}

// legacyKeys opens values an earlier release sealed with encryptionKey: the
// local key, and any retired one, since that key may have been retired
// since.
func legacyKeys(cfg config.LocalEncryptionSettings) ([]cipher.AEAD, error) {
	var out []cipher.AEAD
	for i, raw := range append([]string{cfg.Key}, cfg.RetiredKeys...) {
		setting := "encryption.local.key"
		if i > 0 {
			setting = fmt.Sprintf("encryption.local.retiredKeys[%d]", i-1)
		}
		key, err := decodeLocalKey(setting, raw)
		if err != nil {
			return nil, err
		}
		aead, err := newLegacyAEAD(key)
		if err != nil {
			return nil, err
		}
		out = append(out, aead)
	}
	return out, nil
}

// resolveFile reads a file: setting's file, trimmed.
func resolveFile(setting, value string) (string, error) {
	path, ok := strings.CutPrefix(strings.TrimSpace(value), "file:")
	if !ok {
		return value, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", setting, err)
	}
	return strings.TrimSpace(string(data)), nil
}
