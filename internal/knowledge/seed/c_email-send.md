---
id: c_email-send
topic: connectors
title: Send email through a connected Google or Microsoft account
created_by: seed
sources: [https://developers.google.com/gmail/api/reference/rest/v1/users.messages/send, https://learn.microsoft.com/en-us/graph/api/user-sendmail]
tags: [email, oauth, connectors, side-effects, gmail, outlook]
gold: true
citation_count: 0
---

# Send email through a connected Google or Microsoft account

Use `sdk/email` instead of building Gmail or Microsoft Graph requests by
hand. The account is connected once on the Connections page; the workflow
references it by connection id and the host hands back a fresh, auto-refreshed
access token at run time. The AI builder never sees the token, only the
connection's metadata (its name).

## Pattern

```go
import "github.com/bright-interaction/reactor/sdk/email"

_, err := reactor.Step(flow, ctx, "send-welcome", reactor.StepOpts{
    IdempotencyKey: "welcome:" + in.CustomerID, // sending is a side effect
    Timeout:        30 * time.Second,
}, func(ctx context.Context) (string, error) {
    tok := vault.MustGet("oauth:" + in.GmailConnectionID)
    return email.SendGmail(ctx, string(tok.Reveal()), email.Message{
        From:    in.SenderAddress,
        To:      []string{in.CustomerEmail},
        Subject: "Welcome aboard",
        Text:    "Thanks for signing up.",
        HTML:    "<p>Thanks for signing up.</p>", // optional; both -> multipart/alternative
    })
})
```

## Rules

- The credential id is `oauth:<connection-id>`, not a plain key name. The host
  resolves it to a live token scoped to the workflow's tenant.
- Sending email is a side effect: always set a deterministic `IdempotencyKey`
  (see [[i_step-keys]]) so a replay does not send twice.
- Google: `email.SendGmail` (returns the message id), scope `gmail.send`.
  Microsoft: `email.SendOutlook` (returns no id, Graph replies 202), scope
  `Mail.Send`. `email.Send(provider, ...)` dispatches when you have the provider.
- Do not put the token in a log, a comment, or the dag.json. `Reveal()` is the
  only place the value appears, and only at run time.
