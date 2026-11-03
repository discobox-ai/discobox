package config

import (
	"fmt"
	"strings"
	"time"
)

// The providers encryption.provider and encryption.previous name (ADR
// 26-11-02-902 §3).
const (
	EncryptionLocal         = "local"
	EncryptionAWSKMS        = "awskms"
	EncryptionGCPKMS        = "gcpkms"
	EncryptionAzureKeyVault = "azurekeyvault"
	EncryptionVaultTransit  = "vaulttransit"
)

// The ways encryption.vaulttransit.auth logs in to Vault or OpenBao.
const (
	VaultAuthToken      = "token"
	VaultAuthKubernetes = "kubernetes"
)

// EncryptionSettings is how the server seals what it stores encrypted: each
// project's values with that project's own data key, wrapped by the
// configured provider (ADR 26-11-02-902). No provider stores values
// unsealed.
//
// A cloud provider's credentials are not here: each comes from its SDK's
// own chain, a workload identity or that cloud's own variables.
type EncryptionSettings struct {
	Provider string        `yaml:"provider" env:"DISCOBOX_ENCRYPTION_PROVIDER" enum:"local,awskms,gcpkms,azurekeyvault,vaulttransit" doc:"What wraps each project's data key: local (a key in this configuration), awskms, gcpkms, azurekeyvault, or vaulttransit (Vault or OpenBao). Unset, stored secrets are not sealed." example:"awskms"`
	Previous string        `yaml:"previous" env:"DISCOBOX_ENCRYPTION_PREVIOUS" enum:"local,awskms,gcpkms,azurekeyvault,vaulttransit" doc:"The provider being moved away from. It unwraps data keys but never wraps one, and startup rewraps every data key it wrapped with provider. Remove it once the log says no key is left on it. Its own settings stay configured until then." example:"local"`
	CacheTTL time.Duration `yaml:"cacheTTL" env:"DISCOBOX_ENCRYPTION_CACHE_TTL" default:"5m" doc:"How long a project's unwrapped data key is kept in memory, and so how long a project stays readable here after its key is destroyed or the provider stops unwrapping it. Negative keeps none, and every read calls the provider."`

	Local         LocalEncryptionSettings         `yaml:"local" doc:"The local provider: a key held in this configuration."`
	AWSKMS        AWSKMSEncryptionSettings        `yaml:"awskms" doc:"The AWS KMS provider. Credentials come from the AWS SDK's default chain: IRSA or EKS Pod Identity, an instance profile, or AWS_* variables."`
	GCPKMS        GCPKMSEncryptionSettings        `yaml:"gcpkms" doc:"The Google Cloud KMS provider. Credentials come from Application Default Credentials: GKE Workload Identity, the metadata server, or GOOGLE_APPLICATION_CREDENTIALS."`
	AzureKeyVault AzureKeyVaultEncryptionSettings `yaml:"azurekeyvault" doc:"The Azure Key Vault provider. Credentials come from DefaultAzureCredential: AKS Workload Identity, a managed identity, or AZURE_* variables."`
	VaultTransit  VaultTransitEncryptionSettings  `yaml:"vaulttransit" doc:"The Vault or OpenBao transit provider."`
}

// LocalEncryptionSettings configures the local provider.
type LocalEncryptionSettings struct {
	Key         string   `yaml:"key" env:"DISCOBOX_ENCRYPTION_LOCAL_KEY" doc:"Base64 of the 32-byte key that wraps each project's data key; generate one with: openssl rand -base64 32. file:<path> reads it from a file. Keep it: nothing sealed under it can be read without it. It also opens values an earlier release sealed with encryptionKey." example:"file:/run/secrets/discobox-key"`
	RetiredKeys []string `yaml:"retiredKeys" env:"DISCOBOX_ENCRYPTION_LOCAL_RETIRED_KEYS" doc:"Earlier local keys, each base64 or file:<path>. They unwrap but never wrap, and startup rewraps what they wrapped with key." example:"[file:/run/secrets/discobox-key-2025]"`
}

// AWSKMSEncryptionSettings configures the AWS KMS provider.
type AWSKMSEncryptionSettings struct {
	Key    string `yaml:"key" env:"DISCOBOX_ENCRYPTION_AWSKMS_KEY" doc:"The symmetric KMS key: a key ARN, an alias ARN, a key ID, or alias/<name>." example:"arn:aws:kms:us-east-1:111122223333:alias/discobox"`
	Region string `yaml:"region" env:"DISCOBOX_ENCRYPTION_AWSKMS_REGION" doc:"The key's region. Taken from an ARN; otherwise from the AWS SDK's own configuration (AWS_REGION) when unset." example:"us-east-1"`
}

// GCPKMSEncryptionSettings configures the Google Cloud KMS provider.
type GCPKMSEncryptionSettings struct {
	Key string `yaml:"key" env:"DISCOBOX_ENCRYPTION_GCPKMS_KEY" doc:"The ENCRYPT_DECRYPT crypto key's resource name." example:"projects/my-project/locations/global/keyRings/discobox/cryptoKeys/discobox"`
}

// AzureKeyVaultEncryptionSettings configures the Azure Key Vault provider.
type AzureKeyVaultEncryptionSettings struct {
	VaultURL string `yaml:"vaultURL" env:"DISCOBOX_ENCRYPTION_AZUREKEYVAULT_VAULT_URL" doc:"The vault's URL." example:"https://example.vault.azure.net"`
	Key      string `yaml:"key" env:"DISCOBOX_ENCRYPTION_AZUREKEYVAULT_KEY" doc:"The name of the RSA or oct-HSM key, permitted wrapKey and unwrapKey. Its newest version wraps." example:"discobox"`
}

// VaultTransitEncryptionSettings configures the Vault or OpenBao transit
// provider. Neither has an ambient identity, so it is configured in full
// here.
type VaultTransitEncryptionSettings struct {
	Address   string `yaml:"address" env:"DISCOBOX_ENCRYPTION_VAULTTRANSIT_ADDRESS" doc:"The server's address." example:"https://vault.internal:8200"`
	Namespace string `yaml:"namespace" env:"DISCOBOX_ENCRYPTION_VAULTTRANSIT_NAMESPACE" doc:"The Vault Enterprise or OpenBao namespace, if any." example:"discobox"`
	Mount     string `yaml:"mount" env:"DISCOBOX_ENCRYPTION_VAULTTRANSIT_MOUNT" default:"transit" doc:"Where the transit secrets engine is mounted."`
	Key       string `yaml:"key" env:"DISCOBOX_ENCRYPTION_VAULTTRANSIT_KEY" doc:"The transit key: aes256-gcm96, aes128-gcm96 or chacha20-poly1305, and not derived." example:"discobox"`
	Auth      string `yaml:"auth" env:"DISCOBOX_ENCRYPTION_VAULTTRANSIT_AUTH" enum:"token,kubernetes" default:"token" doc:"How the server logs in: token uses token; kubernetes logs in at auth/kubernetes with this pod's service-account token as role, and renews what it is given."`
	Token     string `yaml:"token" env:"DISCOBOX_ENCRYPTION_VAULTTRANSIT_TOKEN" doc:"The token, for auth: token. file:<path> reads it from a file." example:"file:/run/secrets/vault-token"`
	Role      string `yaml:"role" env:"DISCOBOX_ENCRYPTION_VAULTTRANSIT_ROLE" doc:"The role to log in as, for auth: kubernetes." example:"discobox"`
}

// applyLegacyEncryptionKey moves encryptionKey, the setting
// encryption.local.key replaces, to where it now lives (ADR 26-11-02-902
// §3). A server that sets only encryptionKey keeps running as it did, on the
// local provider with that key.
func (c *Config) applyLegacyEncryptionKey(configured func(string) bool) error {
	if !configured("encryptionKey") || strings.TrimSpace(c.EncryptionKey) == "" {
		return nil
	}
	if strings.TrimSpace(c.Encryption.Local.Key) != "" {
		return fmt.Errorf("encryptionKey and encryption.local.key are both set: encryptionKey is retired, so move its value to encryption.local.key and remove it")
	}
	c.Encryption.Local.Key = c.EncryptionKey
	c.EncryptionKey = ""
	if c.Encryption.Provider == "" {
		c.Encryption.Provider = EncryptionLocal
		c.Warnings = append(c.Warnings, "encryptionKey is retired: move its value to encryption.local.key, with encryption.provider: local")
		return nil
	}
	// With another provider the key only opens what an earlier release
	// sealed, until startup has sealed it all again.
	c.Warnings = append(c.Warnings, "encryptionKey is retired: move its value to encryption.local.key, which opens what an earlier release sealed with it")
	return nil
}

// validate rejects an encryption block the server cannot build a keyring
// from. Whether a key exists, and can wrap, is the provider's to say at
// startup.
func (e EncryptionSettings) validate() error {
	if e.Provider == "" {
		if e.Previous != "" {
			return fmt.Errorf("encryption.previous is set but encryption.provider is not: there is nothing to move to")
		}
		return nil
	}
	if e.Previous == e.Provider {
		return fmt.Errorf("encryption.previous and encryption.provider are both %s: previous names the provider being moved away from", e.Provider)
	}
	for _, name := range []string{e.Provider, e.Previous} {
		if name == "" {
			continue
		}
		if err := e.validateProvider(name); err != nil {
			return err
		}
	}
	return nil
}

func (e EncryptionSettings) validateProvider(name string) error {
	required := func(key, value string) error {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("encryption.%s is required for the %s provider", key, name)
		}
		return nil
	}
	switch name {
	case EncryptionLocal:
		return required("local.key", e.Local.Key)
	case EncryptionAWSKMS:
		return required("awskms.key", e.AWSKMS.Key)
	case EncryptionGCPKMS:
		return required("gcpkms.key", e.GCPKMS.Key)
	case EncryptionAzureKeyVault:
		if err := required("azurekeyvault.vaultURL", e.AzureKeyVault.VaultURL); err != nil {
			return err
		}
		return required("azurekeyvault.key", e.AzureKeyVault.Key)
	case EncryptionVaultTransit:
		v := e.VaultTransit
		if err := required("vaulttransit.address", v.Address); err != nil {
			return err
		}
		if err := required("vaulttransit.key", v.Key); err != nil {
			return err
		}
		switch v.Auth {
		case VaultAuthToken:
			return required("vaulttransit.token", v.Token)
		case VaultAuthKubernetes:
			return required("vaulttransit.role", v.Role)
		default:
			return fmt.Errorf("encryption.vaulttransit.auth must be one of: token, kubernetes")
		}
	default:
		return fmt.Errorf("encryption provider %q is not one of: local, awskms, gcpkms, azurekeyvault, vaulttransit", name)
	}
}
