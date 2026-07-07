---
id: c_generic-api
topic: connectors
title: Integrate any catalog service (CRM, project management, ...) via sdk/http
created_by: seed
sources: []
tags: [connectors, crm, project-management, http, oauth, api-key, idempotency]
gold: true
citation_count: 0
---

# Integrate any catalog service via sdk/http

Most services do not need a dedicated SDK package. The Environment context
lists, for every connected service, its base URL, auth scheme, and canonical
operations, so you call it directly through `sdk/http` against verified
metadata instead of guessing. Prefer a dedicated package (`sdk/email`,
`sdk/stripe`, `sdk/mollie`) when one is listed.

## The pattern

```go
import ahttp "github.com/brightinteraction/reactor/sdk/http"

_, err := reactor.Step(flow, ctx, "create-contact", reactor.StepOpts{
    IdempotencyKey: "hubspot-contact:" + in.Email, // writes are side effects
    Timeout:        30 * time.Second,
}, func(ctx context.Context) (map[string]any, error) {
    // API-key service: the key is a static vault credential by name.
    c := &ahttp.Client{Bearer: string(vault.MustGet("hubspot-key").Reveal())}
    body := map[string]any{"properties": map[string]any{"email": in.Email}}
    var out map[string]any
    if err := c.PostJSON(ctx, "https://api.hubapi.com/crm/v3/objects/contacts", body, &out); err != nil {
        return nil, err
    }
    return out, nil
})
```

## Auth shapes (from the catalog)

- **Bearer key**: `&ahttp.Client{Bearer: string(vault.MustGet("<key-name>").Reveal())}`.
- **OAuth connection**: same, but `vault.MustGet("oauth:<connection-id>")` (the
  host refreshes it for you).
- **Query-param key** (Trello): append `?key=&token=` to the URL; do not set Bearer.
- **HTTP Basic** (Jira, Close): set it via the client's Headers map:
  `&ahttp.Client{Headers: map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+key))}}`.
- **Custom header** (ClickUp `Authorization: <token>` with no Bearer prefix,
  Storyblok, Shortcut `Shortcut-Token`) and **version pins** (Notion
  `Notion-Version`): use the Headers map too:
  `&ahttp.Client{Headers: map[string]string{"Authorization": token, "Notion-Version": "2022-06-28"}}`.
  Values in Headers win over the Bearer default, so mix and match freely.

## Rules

- Every create/update/delete is a side effect: set a deterministic Step
  `IdempotencyKey` (see [[i_step-keys]]).
- Classify failures: wrap transient ones with `reactor.Retryable`, permanent
  4xx (bad input, auth) with `reactor.Permanent`. `ahttp.IsRetryable(err)` helps.
- Never log the key. `Reveal()` is the only place the plaintext appears, and
  only at run time.
- Confirm version- or instance-specific details (Salesforce instance_url, Jira
  domain, Zoho region) against the service's docs link in the Environment.
