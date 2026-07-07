package notifier

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/brightinteraction/reactor/internal/runtime/journal"
)

// resolveChannelSecrets rewrites a channel's config JSON, replacing vault
// credential references with the resolved plaintext just before the send.
// This keeps the secret (SMTP password, webhook auth header) out of the
// config_json column at rest; it only ever materialises in memory for the
// duration of one send.
//
// Recognised references:
//
//	email_smtp      "password_credential_id"   -> sets "password"
//	generic_webhook "header_credential_id" (+  -> sets headers[header_name]
//	                 "header_name")
//
// A config with no credential reference is returned unchanged. An
// unresolvable reference returns an error so the caller can fail closed.
func resolveChannelSecrets(ctx context.Context, kind string, cfg json.RawMessage, resolve func(ctx context.Context, credentialID string) (string, error)) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal(cfg, &m); err != nil {
		return cfg, fmt.Errorf("notifier: parse channel config: %w", err)
	}
	changed := false
	switch kind {
	case journal.ChannelKindEmailSMTP:
		if id, _ := m["password_credential_id"].(string); id != "" {
			v, err := resolve(ctx, id)
			if err != nil {
				return cfg, fmt.Errorf("notifier: resolve smtp password credential %q: %w", id, err)
			}
			m["password"] = v
			changed = true
		}
	case journal.ChannelKindGenericWebhook:
		if id, _ := m["header_credential_id"].(string); id != "" {
			name, _ := m["header_name"].(string)
			if name == "" {
				return cfg, fmt.Errorf("notifier: header_credential_id set but header_name is empty")
			}
			v, err := resolve(ctx, id)
			if err != nil {
				return cfg, fmt.Errorf("notifier: resolve webhook header credential %q: %w", id, err)
			}
			hdrs, _ := m["headers"].(map[string]any)
			if hdrs == nil {
				hdrs = map[string]any{}
			}
			hdrs[name] = v
			m["headers"] = hdrs
			changed = true
		}
	}
	if !changed {
		return cfg, nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return cfg, fmt.Errorf("notifier: re-marshal channel config: %w", err)
	}
	return out, nil
}
