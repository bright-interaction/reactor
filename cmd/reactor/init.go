package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// cmdInit bootstraps a fresh Reactor install:
//
//   - Creates ~/.reactor/ (or --root <dir>) with mode 0700.
//   - Generates a 32-byte master key, hex-encoded, written to
//     master.key with mode 0600.
//   - Creates the workflows/ subdir for compiled binaries.
//   - Creates an empty reactor.db that `reactor migrate` then
//     stamps with the schema.
//
// Idempotent: if master.key already exists, init refuses to overwrite
// and exits 1 so an operator never accidentally clobbers a key that's
// encrypting live credentials.
func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	root := fs.String("root", defaultRoot(), "Reactor state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *root == "" {
		return errors.New("init: --root required (and HOME unset)")
	}

	if err := os.MkdirAll(*root, 0o700); err != nil {
		return fmt.Errorf("init: mkdir %s: %w", *root, err)
	}
	if err := os.MkdirAll(filepath.Join(*root, "workflows"), 0o700); err != nil {
		return fmt.Errorf("init: mkdir workflows: %w", err)
	}

	keyPath := filepath.Join(*root, "master.key")
	if _, err := os.Stat(keyPath); err == nil {
		return fmt.Errorf("init: %s already exists; refusing to overwrite", keyPath)
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Errorf("init: rand: %w", err)
	}
	hexed := hex.EncodeToString(buf)
	if err := os.WriteFile(keyPath, []byte(hexed+"\n"), 0o600); err != nil {
		return fmt.Errorf("init: write master.key: %w", err)
	}

	fmt.Printf("Reactor initialised at %s\n\n", *root)
	fmt.Printf("Master key written to %s (mode 0600)\n", keyPath)
	fmt.Printf("Database path: %s/reactor.db\n\n", *root)
	fmt.Println("Next steps:")
	fmt.Printf("  reactor migrate --db sqlite://%s/reactor.db\n", *root)
	fmt.Printf("  reactor serve   --db sqlite://%s/reactor.db --master-key-file %s/master.key\n", *root, *root)
	return nil
}

// defaultRoot returns ~/.reactor or "" if HOME is unset.
func defaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".reactor")
}
