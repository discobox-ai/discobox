// Command discobox-secret-dump is a development tool that prints a stored
// secret's plaintext value. It exists to inspect secret material that the API
// deliberately never returns, so it must only ever be run by an operator with
// direct access to the database and encryption key.
//
// It resolves the database DSN and DISCOBOX_ENCRYPTION_KEY exactly as
// discobox-server does (including a local .env file), opens the database
// read-only without migrating it, looks the secret up by ID, and decrypts it
// through the same store logic the server uses.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/joho/godotenv"
	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/server/internal/config"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/secrets"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	jsonOut := flag.Bool("json", false, "print the full SecretValue as JSON instead of only populated fields")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [--json] <secret-id>\n\n", os.Args[0])
		fmt.Fprintln(flag.CommandLine.Output(),
			"Prints a stored secret's decrypted value. Reads DATABASE_DSN /\n"+
				"DISCOBOX_ENCRYPTION_KEY (and .env) the same way discobox-server does.\n"+
				"The secret ID may be a full ID or a unique prefix.")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		return errors.New("exactly one secret ID argument is required")
	}
	secretID := strings.TrimSpace(flag.Arg(0))
	if secretID == "" {
		return errors.New("secret ID must not be empty")
	}

	// Match discobox-server: pick up a local .env, then load config from the
	// environment so the DSN and encryption key resolve identically.
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	db, err := database.New(database.Config{
		Driver:  cfg.DatabaseDriver,
		DSN:     cfg.DatabaseDSN,
		ReadDSN: cfg.DatabaseReadDSN,
	})
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	secret, err := findSecret(ctx, db.Read, secretID)
	if err != nil {
		return err
	}

	var sealer secrets.Sealer
	if cfg.EncryptionKey != "" {
		sealer, err = secrets.NewAESGCMSealerFromBase64Key(cfg.EncryptionKey)
		if err != nil {
			return fmt.Errorf("initialize encryption: %w", err)
		}
	}

	// Reuse the server's own decrypt-and-deserialize path so the purpose and
	// resource-ID binding stay in sync with how the value was sealed. A nil
	// sealer transparently handles plaintext rows written before encryption was
	// enabled (or when DISCOBOX_ENCRYPTION_KEY is unset).
	appStore := store.New(db.Write, db.Read, store.WithSealer(sealer))
	value, err := appStore.OpenSecretValue(ctx, secret)
	if err != nil {
		if sealer == nil && secrets.IsSealed(secret.EncryptedValue) {
			return fmt.Errorf("secret %s is encrypted but DISCOBOX_ENCRYPTION_KEY is not set: %w", secret.ID, err)
		}
		return fmt.Errorf("decrypt secret %s: %w", secret.ID, err)
	}
	if value == nil {
		return fmt.Errorf("secret %s has no stored value", secret.ID)
	}

	fmt.Fprintf(os.Stderr, "secret %s  project=%s  name=%q  type=%s  host=%q\n",
		secret.ID, secret.ProjectID, secret.Name, secret.Type, secret.Host)

	if *jsonOut {
		// Emitting the plaintext secret is this tool's entire purpose.
		out, err := json.MarshalIndent(value, "", "  ") //nolint:gosec // G117: intentional secret dump
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	printValue(secret.Type, value)
	return nil
}

// findSecret looks a secret up by exact ID, falling back to a unique prefix
// match so operators can pass a shortened ID. It reads soft-deleted rows too,
// though DeleteSecret nulls the ciphertext, so those decrypt to nothing.
func findSecret(ctx context.Context, read *gorm.DB, idOrPrefix string) (*model.Secret, error) {
	var secret model.Secret
	err := read.WithContext(ctx).Where("id = ?", idOrPrefix).First(&secret).Error
	if err == nil {
		return &secret, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("query secret: %w", err)
	}

	var matches []model.Secret
	if err := read.WithContext(ctx).Where("id LIKE ?", idOrPrefix+"%").Limit(2).Find(&matches).Error; err != nil {
		return nil, fmt.Errorf("query secret by prefix: %w", err)
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no secret found with ID or prefix %q", idOrPrefix)
	case 1:
		return &matches[0], nil
	default:
		return nil, fmt.Errorf("secret ID prefix %q is ambiguous; use the full ID", idOrPrefix)
	}
}

// printValue writes only the fields relevant to the secret's type. Each value
// is printed on its own line, unquoted, so it can be piped or copied verbatim.
func printValue(secretType string, v *model.SecretValue) {
	emit := func(label, val string) {
		if val != "" {
			fmt.Printf("%s: %s\n", label, val)
		}
	}
	switch secretType {
	case model.SecretTypeGit:
		emit("username", v.Username)
		emit("password", v.Password)
	case model.SecretTypeSSH:
		emit("privateKey", v.PrivateKey)
		emit("passphrase", v.Passphrase)
	case model.SecretTypeBearer:
		emit("token", v.Token)
	default:
		// Unknown type: dump whatever is populated.
		emit("username", v.Username)
		emit("password", v.Password)
		emit("privateKey", v.PrivateKey)
		emit("passphrase", v.Passphrase)
		emit("token", v.Token)
	}
}
