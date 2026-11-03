package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadEncryptionDefaultsToUnsealed(t *testing.T) {
	clearConfigEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Encryption.Provider != "" || cfg.Encryption.CacheTTL != 5*time.Minute || cfg.Encryption.VaultTransit.Mount != "transit" || cfg.Encryption.VaultTransit.Auth != VaultAuthToken {
		t.Fatalf("Encryption = %+v, want no provider, a 5m cache, mount transit, token auth", cfg.Encryption)
	}
}

func TestLoadEncryptionFromTheEnvironment(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DISCOBOX_ENCRYPTION_PROVIDER", "awskms")
	t.Setenv("DISCOBOX_ENCRYPTION_PREVIOUS", "local")
	t.Setenv("DISCOBOX_ENCRYPTION_AWSKMS_KEY", "alias/discobox")
	t.Setenv("DISCOBOX_ENCRYPTION_LOCAL_KEY", "file:/run/secrets/key")
	t.Setenv("DISCOBOX_ENCRYPTION_CACHE_TTL", "1m")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	e := cfg.Encryption
	if e.Provider != EncryptionAWSKMS || e.Previous != EncryptionLocal || e.AWSKMS.Key != "alias/discobox" || e.Local.Key != "file:/run/secrets/key" || e.CacheTTL != time.Minute {
		t.Fatalf("Encryption = %+v", e)
	}
}

func TestLoadEncryptionKeyIsMovedToTheLocalProviderWithAWarning(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DISCOBOX_ENCRYPTION_KEY", "a2V5")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Encryption.Provider != EncryptionLocal || cfg.Encryption.Local.Key != "a2V5" {
		t.Fatalf("Encryption = %+v, want local with the old key", cfg.Encryption)
	}
	if !strings.Contains(strings.Join(cfg.Warnings, "\n"), "encryption.local.key") {
		t.Fatalf("Warnings = %q, want one naming encryption.local.key", cfg.Warnings)
	}
}

// With another provider configured, the old key stays only to open what it
// sealed, and the warning does not tell the operator to switch to local.
func TestLoadEncryptionKeyBesideAnotherProviderKeepsThatProvider(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DISCOBOX_ENCRYPTION_KEY", "a2V5")
	t.Setenv("DISCOBOX_ENCRYPTION_PROVIDER", "gcpkms")
	t.Setenv("DISCOBOX_ENCRYPTION_GCPKMS_KEY", "k")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Encryption.Provider != EncryptionGCPKMS || cfg.Encryption.Local.Key != "a2V5" {
		t.Fatalf("Encryption = %+v, want gcpkms with the old key as the local key", cfg.Encryption)
	}
	warnings := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(warnings, "encryption.local.key") || strings.Contains(warnings, "provider: local") {
		t.Fatalf("Warnings = %q, want one naming encryption.local.key and not switching to local", cfg.Warnings)
	}
}

func TestLoadRejectsUnusableEncryption(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"both keys", map[string]string{"DISCOBOX_ENCRYPTION_KEY": "a", "DISCOBOX_ENCRYPTION_LOCAL_KEY": "b"}, "both set"},
		{"previous alone", map[string]string{"DISCOBOX_ENCRYPTION_PREVIOUS": "local", "DISCOBOX_ENCRYPTION_LOCAL_KEY": "b"}, "nothing to move to"},
		{"previous is provider", map[string]string{"DISCOBOX_ENCRYPTION_PROVIDER": "local", "DISCOBOX_ENCRYPTION_PREVIOUS": "local", "DISCOBOX_ENCRYPTION_LOCAL_KEY": "b"}, "both local"},
		{"unknown provider", map[string]string{"DISCOBOX_ENCRYPTION_PROVIDER": "rot13"}, "not one of"},
		{"local without a key", map[string]string{"DISCOBOX_ENCRYPTION_PROVIDER": "local"}, "encryption.local.key"},
		{"previous without its settings", map[string]string{"DISCOBOX_ENCRYPTION_PROVIDER": "gcpkms", "DISCOBOX_ENCRYPTION_GCPKMS_KEY": "k", "DISCOBOX_ENCRYPTION_PREVIOUS": "local"}, "encryption.local.key"},
		{"azure without a vault", map[string]string{"DISCOBOX_ENCRYPTION_PROVIDER": "azurekeyvault", "DISCOBOX_ENCRYPTION_AZUREKEYVAULT_KEY": "k"}, "vaultURL"},
		{"vault token auth without a token", map[string]string{"DISCOBOX_ENCRYPTION_PROVIDER": "vaulttransit", "DISCOBOX_ENCRYPTION_VAULTTRANSIT_ADDRESS": "http://v", "DISCOBOX_ENCRYPTION_VAULTTRANSIT_KEY": "k"}, "vaulttransit.token"},
		{"vault kubernetes auth without a role", map[string]string{"DISCOBOX_ENCRYPTION_PROVIDER": "vaulttransit", "DISCOBOX_ENCRYPTION_VAULTTRANSIT_ADDRESS": "http://v", "DISCOBOX_ENCRYPTION_VAULTTRANSIT_KEY": "k", "DISCOBOX_ENCRYPTION_VAULTTRANSIT_AUTH": "kubernetes"}, "vaulttransit.role"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clearConfigEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}
