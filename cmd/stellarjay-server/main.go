package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	stellarjay "github.com/kyle-visner/stellarjay"
	"github.com/kyle-visner/stellarjay/server"
)

func main() {
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	var err error
	switch command {
	case "serve":
		err = serve()
	case "healthcheck":
		err = healthcheck()
	case "hash-token":
		err = hashToken()
	case "add-token":
		err = addToken()
	case "revoke-token":
		err = revokeToken()
	case "catalog":
		err = catalogCommand()
	case "migrate-key":
		err = migrateKey()
	case "init":
		err = initSecrets()
	default:
		err = fmt.Errorf("unknown command %q (expected serve, healthcheck, hash-token, add-token, revoke-token, catalog, migrate-key, or init)", command)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "stellarjay-server:", err)
		os.Exit(1)
	}
}

func serve() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	dataDir := envOr("STELLARJAY_DATA_DIR", stellarjay.DefaultDir())
	authFile := stellarjay.Getenv("STELLARJAY_AUTH_FILE")
	keyFile := stellarjay.Getenv("STELLARJAY_DATA_KEY_FILE")
	if authFile == "" {
		return fmt.Errorf("STELLARJAY_AUTH_FILE is required")
	}
	if keyFile == "" {
		return fmt.Errorf("STELLARJAY_DATA_KEY_FILE is required; hosted mode never stores its key with the data")
	}
	encodedKey, err := os.ReadFile(keyFile)
	if err != nil {
		return fmt.Errorf("read STELLARJAY_DATA_KEY_FILE: %w", err)
	}
	store, err := stellarjay.OpenStoreWithDataKey(dataDir, strings.TrimSpace(string(encodedKey)))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer store.Close()
	auth, err := server.LoadAuthenticator(authFile)
	if err != nil {
		return err
	}
	var catalog *server.Catalog
	if catalogFile := stellarjay.Getenv("STELLARJAY_CATALOG_FILE"); catalogFile != "" {
		catalog, err = server.LoadCatalog(catalogFile)
		if err != nil {
			return err
		}
	}
	snapshotRetention, err := envInt("STELLARJAY_SNAPSHOT_RETENTION", 24)
	if err != nil {
		return err
	}
	snapshotMinFreeBytes, err := envUint64("STELLARJAY_SNAPSHOT_MIN_FREE_BYTES", 512<<20)
	if err != nil {
		return err
	}
	rateLimit, err := envInt("STELLARJAY_RATE_LIMIT_PER_MINUTE", 600)
	if err != nil {
		return err
	}
	failedAuthLimit, err := envInt("STELLARJAY_FAILED_AUTH_LIMIT_PER_MINUTE", 30)
	if err != nil {
		return err
	}
	api, err := server.New(server.Options{
		Store: store, Auth: auth, BackupDir: stellarjay.Getenv("STELLARJAY_BACKUP_DIR"), Logger: logger,
		SnapshotRetention: snapshotRetention, SnapshotMinFreeBytes: snapshotMinFreeBytes,
		MinimumRoot:        stellarjay.Getenv("STELLARJAY_MINIMUM_ROOT"),
		RateLimitPerMinute: rateLimit, FailedAuthLimitPerMinute: failedAuthLimit,
		Catalog: catalog,
	})
	if err != nil {
		return err
	}
	address := envOr("STELLARJAY_LISTEN_ADDR", "127.0.0.1:8080")
	httpServer := &http.Server{
		Addr: address, Handler: api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	shutdownContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdownContext.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("stellarjay ready", "address", address, "data_dir", dataDir,
		"backups_enabled", stellarjay.Getenv("STELLARJAY_BACKUP_DIR") != "")
	err = httpServer.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func envInt(name string, fallback int) (int, error) {
	raw := stellarjay.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func envUint64(name string, fallback uint64) (uint64, error) {
	raw := stellarjay.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return value, nil
}

func healthcheck() error {
	url := "http://127.0.0.1:8080/health/ready"
	if len(os.Args) > 2 {
		url = os.Args[2]
	}
	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("health check returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func hashToken() error {
	stat, err := os.Stdin.Stat()
	if err != nil {
		return err
	}
	if stat.Mode()&os.ModeCharDevice != 0 {
		fmt.Fprint(os.Stderr, "Token: ")
	}
	token, err := bufio.NewReader(io.LimitReader(os.Stdin, 1025)).ReadString('\n')
	if err != nil && err != io.EOF {
		return err
	}
	token = strings.TrimSpace(token)
	if len(token) < 32 {
		return fmt.Errorf("token must be at least 32 characters")
	}
	sum := sha256.Sum256([]byte(token))
	fmt.Println(hex.EncodeToString(sum[:]))
	return nil
}

func addToken() error {
	args := os.Args[2:]
	if len(args) < 3 {
		return fmt.Errorf("usage: stellarjay-server add-token AUTH_FILE ID ROLE [NOT_AFTER_RFC3339] [--allow-type PATTERN]... [--allow-command COMMAND]... [--allow-ref PATTERN]...")
	}
	var notAfter *time.Time
	rest := args[3:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "--") {
		parsed, err := time.Parse(time.RFC3339, rest[0])
		if err != nil {
			return fmt.Errorf("NOT_AFTER must be RFC3339: %w", err)
		}
		notAfter = &parsed
		rest = rest[1:]
	}
	allow, err := parseAllowFlags(rest)
	if err != nil {
		return err
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	if err := server.AddToken(args[0], args[1], args[2], token, notAfter, allow); err != nil {
		return err
	}
	fmt.Printf("%s=%s\n", args[1], token)
	fmt.Fprintln(os.Stderr, "Token added. Store the plaintext in a password manager, then recreate the service to load the updated auth file.")
	return nil
}

func parseAllowFlags(args []string) (*server.Allow, error) {
	if len(args) == 0 {
		return nil, nil
	}
	allow := &server.Allow{}
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			return nil, fmt.Errorf("%s requires a value", args[i])
		}
		value := args[i+1]
		switch args[i] {
		case "--allow-type":
			allow.Types = append(allow.Types, value)
		case "--allow-command":
			allow.Commands = append(allow.Commands, value)
		case "--allow-ref":
			allow.Refs = append(allow.Refs, value)
		default:
			return nil, fmt.Errorf("unknown argument %q", args[i])
		}
		i++
	}
	if len(allow.Types) == 0 && len(allow.Commands) == 0 && len(allow.Refs) == 0 {
		return nil, nil
	}
	return allow, nil
}

func catalogCommand() error {
	if len(os.Args) < 4 {
		return fmt.Errorf("usage: stellarjay-server catalog show|install|remove|enforce CATALOG_FILE ...")
	}
	catalog, err := server.LoadCatalog(os.Args[3])
	if err != nil {
		return err
	}
	switch os.Args[2] {
	case "show":
		if len(os.Args) != 4 {
			return fmt.Errorf("usage: stellarjay-server catalog show CATALOG_FILE")
		}
		return json.NewEncoder(os.Stdout).Encode(catalog.View())
	case "install":
		if len(os.Args) < 6 {
			return fmt.Errorf("usage: stellarjay-server catalog install CATALOG_FILE TYPE COMMAND...")
		}
		entry, _, err := catalog.Install(os.Args[4], os.Args[5:])
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Catalog updated and enforcement is on. Recreate Stellar Jay if it was already running; the HTTP catalog API applies without a recreate.")
		return json.NewEncoder(os.Stdout).Encode(entry)
	case "remove":
		if len(os.Args) != 5 {
			return fmt.Errorf("usage: stellarjay-server catalog remove CATALOG_FILE TYPE")
		}
		if err := catalog.Remove(os.Args[4]); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Removed %s. Enforcement is unchanged. Recreate Stellar Jay if it was already running.\n", os.Args[4])
		return nil
	case "enforce":
		if len(os.Args) != 5 || (os.Args[4] != "true" && os.Args[4] != "false") {
			return fmt.Errorf("usage: stellarjay-server catalog enforce CATALOG_FILE true|false")
		}
		if err := catalog.SetEnforced(os.Args[4] == "true"); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Catalog enforced=%s. Recreate Stellar Jay if it was already running.\n", os.Args[4])
		return nil
	default:
		return fmt.Errorf("unknown catalog command %q", os.Args[2])
	}
}

func revokeToken() error {
	if len(os.Args) != 4 {
		return fmt.Errorf("usage: stellarjay-server revoke-token AUTH_FILE ID")
	}
	if err := server.RevokeToken(os.Args[2], os.Args[3]); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Token revoked. Recreate the service to load the updated auth file.")
	return nil
}

func migrateKey() error {
	if len(os.Args) != 6 {
		return fmt.Errorf("usage: stellarjay-server migrate-key SOURCE_DIR DESTINATION_DIR OLD_KEY_FILE NEW_KEY_FILE")
	}
	oldKey, err := os.ReadFile(os.Args[4])
	if err != nil {
		return fmt.Errorf("read old key file: %w", err)
	}
	newKey, err := os.ReadFile(os.Args[5])
	if err != nil {
		return fmt.Errorf("read new key file: %w", err)
	}
	store, err := stellarjay.OpenStoreWithDataKey(os.Args[2], strings.TrimSpace(string(oldKey)))
	if err != nil {
		return fmt.Errorf("open source store: %w", err)
	}
	defer store.Close()
	result, err := store.MigrateDataKey(os.Args[3], strings.TrimSpace(string(newKey)))
	if err == nil || result.HashMap != nil {
		if encodeErr := json.NewEncoder(os.Stdout).Encode(result); encodeErr != nil {
			return errors.Join(err, encodeErr)
		}
	}
	return err
}

func initSecrets() error {
	dir := "secrets"
	if len(os.Args) > 2 {
		dir = os.Args[2]
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"data_key", "auth.json"} {
		path := filepath.Join(dir, name)
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("refusing to overwrite existing %s", path)
		} else if !os.IsNotExist(err) {
			return err
		}
	}

	key, err := randomDataKey()
	if err != nil {
		return err
	}
	if err := createSecretFile(filepath.Join(dir, "data_key"), []byte(key+"\n")); err != nil {
		return err
	}

	type generatedCredential struct {
		ID     string `json:"id"`
		Role   string `json:"role"`
		SHA256 string `json:"sha256"`
	}
	credentials := make([]generatedCredential, 0, 3)
	plaintext := make(map[string]string, 3)
	for _, role := range []string{"admin", "writer", "reader"} {
		token, err := randomToken()
		if err != nil {
			return err
		}
		id := role + "-initial"
		sum := sha256.Sum256([]byte(token))
		credentials = append(credentials, generatedCredential{
			ID: id, Role: role, SHA256: hex.EncodeToString(sum[:]),
		})
		plaintext[id] = token
	}
	authJSON, err := json.MarshalIndent(map[string]any{"tokens": credentials}, "", "  ")
	if err != nil {
		return err
	}
	if err := createSecretFile(filepath.Join(dir, "auth.json"), append(authJSON, '\n')); err != nil {
		return err
	}
	fmt.Printf("Secrets created in %s. Store these tokens in a password manager; they are not written to disk:\n", dir)
	for _, id := range []string{"admin-initial", "writer-initial", "reader-initial"} {
		fmt.Printf("%s=%s\n", id, plaintext[id])
	}
	return nil
}

func randomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func randomDataKey() (string, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

func createSecretFile(path string, contents []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create %s (refusing to overwrite): %w", path, err)
	}
	if _, err := f.Write(contents); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func envOr(name, fallback string) string {
	if value := stellarjay.Getenv(name); value != "" {
		return value
	}
	return fallback
}
