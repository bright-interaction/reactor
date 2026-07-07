package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// availableClients is the list shown in usage + accepted by --client.
var availableClients = []string{
	"claude-desktop",
	"claude-code",
	"cursor",
	"continue",
	"cline",
	"generic",
}

// cmdMCPInstall prints (or applies, with --apply) the JSON snippet the
// named MCP client expects for reactor. The snippet shape varies per
// client but always boils down to "command: reactor, args: [mcp, stdio,
// --db, ...]"; this helper hides that variance behind one ergonomic
// command.
func cmdMCPInstall(args []string) error {
	fs := flag.NewFlagSet("mcp install", flag.ContinueOnError)
	client := fs.String("client", "", "MCP client name (required): "+strings.Join(availableClients, ", "))
	allowWrite := fs.Bool("allow-write", false, "include --allow-write in the snippet (registers write tools)")
	apply := fs.Bool("apply", false, "modify the client's config file in place (default: print only)")
	dbURL := fs.String("db", envFirst("REACTOR_DB_URL", "ARACHNE_DB_URL"), "$REACTOR_DB_URL embedded in the snippet (default: $REACTOR_DB_URL)")
	root := fs.String("root", defaultRoot(), "Reactor state directory embedded in the snippet")
	binary := fs.String("binary", "", "absolute path to the reactor binary (default: detected from /proc/self/exe)")
	if err := fs.Parse(reorderArgs(args)); err != nil {
		return err
	}
	if *client == "" {
		return errors.New("mcp install: --client required (one of " + strings.Join(availableClients, ", ") + ")")
	}
	if !knownClient(*client) {
		return fmt.Errorf("mcp install: unknown client %q (have: %s)", *client, strings.Join(availableClients, ", "))
	}

	bin := *binary
	if bin == "" {
		exe, err := os.Executable()
		if err == nil {
			bin = exe
		} else {
			bin = "reactor"
		}
	}

	cmdArgs := []string{"mcp", "stdio"}
	if *dbURL != "" {
		cmdArgs = append(cmdArgs, "--db", *dbURL)
	}
	if *root != "" {
		cmdArgs = append(cmdArgs, "--root", *root)
	}
	if *allowWrite {
		cmdArgs = append(cmdArgs, "--allow-write")
	}

	snippet, configPath, err := buildSnippet(*client, bin, cmdArgs)
	if err != nil {
		return err
	}

	pretty, err := json.MarshalIndent(snippet, "", "  ")
	if err != nil {
		return err
	}

	if !*apply {
		fmt.Printf("# reactor MCP snippet for %s\n", *client)
		if configPath != "" {
			fmt.Printf("# (paste into %s, merging with existing mcpServers)\n", configPath)
		}
		fmt.Println(string(pretty))
		return nil
	}
	if configPath == "" {
		return fmt.Errorf("mcp install: --apply not supported for client %q (snippet is freeform)", *client)
	}
	return applyToClientConfig(configPath, snippet)
}

func knownClient(name string) bool {
	for _, c := range availableClients {
		if c == name {
			return true
		}
	}
	return false
}

// buildSnippet returns (snippet-json-shape, default-config-path-for-this-client, error).
// The snippet is always rooted at "mcpServers.reactor" because that's
// the de-facto MCP convention every client we target follows.
func buildSnippet(client, bin string, cmdArgs []string) (any, string, error) {
	server := map[string]any{
		"command": bin,
		"args":    cmdArgs,
	}
	wrapper := map[string]any{
		"mcpServers": map[string]any{
			"reactor": server,
		},
	}
	switch client {
	case "claude-desktop":
		return wrapper, claudeDesktopConfigPath(), nil
	case "claude-code":
		// claude-code reads .claude/settings.json with the same shape.
		return wrapper, ".claude/settings.json", nil
	case "cursor":
		// cursor reads ~/.cursor/mcp.json with the same shape.
		return wrapper, expandHome("~/.cursor/mcp.json"), nil
	case "continue":
		// continue.dev's config.json uses "mcpServers" too (since v0.9).
		return wrapper, expandHome("~/.continue/config.json"), nil
	case "cline":
		// cline reads VS Code settings; tell the operator where without
		// trying to merge VS Code's bigger config file ourselves.
		return wrapper, "(your VS Code settings.json under cline.mcpServers)", nil
	case "generic":
		// Pure stdio block; any 2024-11-05 client can lift this in.
		return server, "", nil
	}
	return nil, "", fmt.Errorf("mcp install: unhandled client %q", client)
}

func claudeDesktopConfigPath() string {
	switch runtime.GOOS {
	case "darwin":
		return expandHome("~/Library/Application Support/Claude/claude_desktop_config.json")
	case "windows":
		return os.ExpandEnv("$APPDATA/Claude/claude_desktop_config.json")
	default:
		return expandHome("~/.config/Claude/claude_desktop_config.json")
	}
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p, "~"))
}

// applyToClientConfig merges reactor into the client's existing
// mcpServers map, backing up the original to <path>.bak-<unix>.
// Refuses if the file isn't shaped as expected (avoids stomping a
// hand-edited config we don't understand).
func applyToClientConfig(path string, snippet any) error {
	wrapper, ok := snippet.(map[string]any)
	if !ok {
		return errors.New("mcp install: snippet shape unexpected")
	}
	servers, _ := wrapper["mcpServers"].(map[string]any)
	if servers == nil {
		return errors.New("mcp install: snippet missing mcpServers")
	}

	var existing map[string]any
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("mcp install: parse %s: %w", path, err)
		}
		// Back up the original before any write.
		bak := fmt.Sprintf("%s.bak-%d", path, time.Now().Unix())
		if err := os.WriteFile(bak, raw, 0o600); err != nil {
			return fmt.Errorf("mcp install: backup: %w", err)
		}
		fmt.Fprintf(os.Stderr, "(backup written to %s)\n", bak)
	} else {
		existing = map[string]any{}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("mcp install: mkdir: %w", err)
		}
	}

	mergedServers, _ := existing["mcpServers"].(map[string]any)
	if mergedServers == nil {
		mergedServers = map[string]any{}
	}
	for k, v := range servers {
		mergedServers[k] = v
	}
	existing["mcpServers"] = mergedServers

	out, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("mcp install: write %s: %w", path, err)
	}
	fmt.Printf("wrote reactor MCP entry to %s\n", path)
	return nil
}
