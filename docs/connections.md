# OAuth connections

Let workflows act as a connected third-party account (Google, Slack, GitHub,
...) without anyone pasting raw API keys. The user connects an account once via
the standard OAuth consent screen; Reactor stores the tokens encrypted and
refreshes them automatically, and a workflow fetches a fresh access token by
connection id.

## 1. Register a provider (admin, once)

On **OAuth providers** (`/oauth-providers`, admin-only):

- Create your OAuth app in the provider's console and set its redirect /
  callback URL to the value shown on the page (`<your-host>/oauth/callback`).
- Fill in the provider id, the authorize + token URLs, the client id +
  secret, and the scopes, then enable it. Common endpoints:
  - **Google** auth `https://accounts.google.com/o/oauth2/v2/auth`, token `https://oauth2.googleapis.com/token`
  - **Slack** auth `https://slack.com/oauth/v2/authorize`, token `https://slack.com/api/oauth.v2.access`
  - **GitHub** auth `https://github.com/login/oauth/authorize`, token `https://github.com/login/oauth/access_token`

Client secrets are encrypted at rest with the master key (the same envelope
encryption the vault uses) and are never shown back. Provider URLs must be
https (http is allowed only on localhost for development).

## 2. Connect an account (any user)

On **Connections** (`/connections`), pick an enabled provider, name the
connection, and click Connect. You are sent to the provider's consent screen
and returned here once you approve. The connection is scoped to your tenant;
its tokens are encrypted and never displayed. The flow uses PKCE and a
single-use, expiring CSRF state.

## 3. Use it in a workflow

Reference the connection from workflow code as the credential id
`oauth:<connection-id>` (the id is shown on the Connections page). The host
resolves it to a **fresh** access token, transparently refreshing via the
refresh token when it has expired:

```go
token, _ := secrets.Get(ctx, "oauth:conn_AbC123")
// use token as the Bearer for the provider's API
```

The same per-workflow secret ACL (`workflow_secret_grants`) that gates vault
credentials gates `oauth:` ids, and the lookup is scoped to the run's tenant,
so a workflow can only use its own tenant's connections.
