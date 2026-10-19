# Mail

Forge's `mail` package sends email. Messages are validated data, delivery goes
through a `Sender` port, and the defaults are the safe ones. The design is in
[ADR 0024](adr/0024-mail.md).

## Configuration

| Variable | Default | Meaning |
|----------|---------|---------|
| `FORGE_MAIL_URL` | empty (mail disabled) | `smtp://user:password@host:587` (STARTTLS), `smtps://user:password@host:465` (implicit TLS) or `file:///path` (development `.eml` files). Percent-encode `@`, `:` and `/` in the password. |
| `FORGE_MAIL_FROM` | required with a URL | Default sender, `app@example.com` or `App <app@example.com>`. |
| `FORGE_MAIL_ALLOW_PLAINTEXT` | `false` | `true` lets the SMTP sender continue when the server offers no STARTTLS. It never allows the password in clear: AUTH without TLS is sent only to `localhost`, 127.0.0.0/8 or `::1` (a local Mailpit); to any other host the send fails. In `production` it cannot be combined with credentials for a remote host. |
| `FORGE_MAIL_TIMEOUT` | `30s` | Bounds one delivery attempt (maximum 10m). |
| `FORGE_MAIL_PREVIEW` | `false` | `true` enables the preview handler. Only valid when `FORGE_ENV` is `development` or `test`. |
| `FORGE_MAIL_TLS_CA_FILE` | empty | PEM file with extra trusted roots for a private relay. There is no setting to skip certificate verification. |
| `FORGE_AUTH_PASSWORD_RESET_URL` | empty | Absolute URL of the reset page (`https://app.example.com/passwords/edit`; http only on localhost). Generated PostgreSQL applications send reset emails, and show the reset pages, only when it and `FORGE_MAIL_URL` are set. |

`production` refuses `file://` and starts only with a valid URL. The SMTP
sender always verifies the server certificate and refuses a server without
STARTTLS unless `FORGE_MAIL_ALLOW_PLAINTEXT=true`.

```go
cfg, err := config.Load()
sender, err := mail.FromConfig(cfg)
switch {
case errors.Is(err, mail.ErrNotConfigured):
    // mail is optional: leave features that need it (recovery) disabled
case err != nil:
    return err
}
from, err := mail.DefaultFrom(cfg)
```

## Sending

```go
msg := mail.Message{
    From:    from,
    To:      []mail.Address{{Name: "Alice", Email: "alice@example.org"}},
    Subject: "Your receipt",
    Text:    "Thanks for your order.",
    HTML:    "<p>Thanks for your order.</p>",
    Attachments: []mail.Attachment{
        {Filename: "receipt.pdf", ContentType: "application/pdf", Data: pdf},
    },
    Headers: map[string]string{"List-Unsubscribe": "<https://example.com/u/42>"},
}
err := sender.Send(ctx, msg)
```

Everything user-controlled is validated by `mail.Encode` before a byte leaves
the process: CR/LF and other control characters are rejected in every header,
address and file name; addresses must be plain ASCII `local@domain`; extra
headers come from an allow-list; recipients, sizes and attachments are
bounded. Errors name the field, never the value.

`Bcc` recipients get the message but appear in no header.

Errors are `fault.Error`s you can match with `errors.Is`: `mail.ErrInvalid`,
`ErrUnavailable` (retryable), `ErrRejected`, `ErrAuthFailed`, `ErrTLSRequired`,
`ErrTLSFailed`, `ErrTemplate`. They never contain credentials, recipient
addresses or server reply text.

## Limits and known limitations

Defaults (override with `mail.WithLimits`): subject 255 runes, 100 recipients,
2 MiB per body, 10 attachments of at most 7 MiB, 10 MiB of content in total.
Messages are held in memory while encoding, so send large files as links.

Addresses must be ASCII: Forge supports neither SMTPUTF8 nor IDNA. An address
such as `joão@exemplo.com` is rejected with `mail.ErrAddressUnsupported`
(`mail_address_unsupported`), never converted, and the recovery adapter logs
that code. Display names may contain any Unicode text.

## Templates

Files for a template named `welcome`, in any `fs.FS` (usually `embed.FS`):

```
welcome.subject.tmpl    optional, one line
welcome.text.tmpl       text/plain part (text/template)
welcome.html.gohtml     text/html part (html/template, contextual escaping)
```

```go
//go:embed mail/*
var mailFS embed.FS

type Welcome struct{ Name, URL string }

tmpl, err := mail.NewTemplate[Welcome](mailFS, "mail/welcome")
err = tmpl.Deliver(ctx, sender, mail.Message{From: from, To: to}, Welcome{Name: name, URL: url})
```

Rendering is buffered: if any part fails, nothing is sent. `WithFuncs`,
`WithTextPartials` and `WithHTMLPartials` add functions and shared layouts.

## Testing

```go
recorder := mailtest.NewRecorder()
svc := NewSignup(recorder)
_ = svc.Register(ctx, "alice@example.org")

deliveries := recorder.Deliveries()   // like ActionMailer::Base.deliveries
got := deliveries[0].Message
```

The recorder runs the real encoder, so a test fails on a message production
would reject. `recorder.FailWith(mail.ErrUnavailable)` simulates an outage.
`Delivery.Parse()` re-reads the exact RFC 5322 bytes with `net/mail`.

## Development

With `FORGE_MAIL_URL=file:tmp/mails` (or `file:///var/tmp/mails`) every message
becomes an `.eml` file (mode 0600) and only its path is logged. For a browser
preview with fixture data, register previews, available only in `development`
and `test` and only with `FORGE_MAIL_PREVIEW=true` (an unset `FORGE_ENV` means
development, so the environment alone is not enough):

```go
if cfg.Mail.Preview { // FORGE_MAIL_PREVIEW=true, off by default
    previews, err := mail.NewPreviewHandler(cfg, "/_mail",
        mail.Preview{Name: "welcome", Description: "After sign-up", Build: buildWelcomePreview})
    mux.Handle("GET /_mail/", previews)
}
```

The preview shows headers, the HTML part in a sandboxed frame, the text part,
inline images, the raw MIME source and the validation error if the message
would not be sent.

## Password recovery

```go
recovery, err := recoverymail.New(sender, recoverymail.Config{
    From:     from,
    AppName:  "Acme",
    ResetURL: "https://app.example.com/reset-password",
    Logger:   logger, // required: failed deliveries are logged here (code and stage only)
})
service, err := auth.NewRecoveryService(store, recovery)
```

Generated PostgreSQL applications do exactly this in
`bootstrap.passwordRecoveryDelivery` when `FORGE_MAIL_URL`, `FORGE_MAIL_FROM`
and `FORGE_AUTH_PASSWORD_RESET_URL` are set, with the sender `FromConfig`
builds (inline SMTP).

`recoverymail.Sender` implements `auth.RecoverySender`. Give it a queue-backed
`mail.Sender` in production: SMTP inline makes the response time reveal whether
an account exists (ADR 0013).

## Deliver later

`mail` has no queue. A queue is a `Sender`:

```go
type QueueSender struct{ jobs Enqueuer }

func (q QueueSender) Send(ctx context.Context, msg mail.Message) error {
    if err := msg.Validate(); err != nil { // fail on the request path
        return err
    }
    return q.jobs.Enqueue(ctx, "mail.send", msg) // Message is JSON-friendly
}

// worker: err := real.Send(ctx, msg)
//   errors.Is(err, mail.ErrUnavailable) -> retry with backoff
//   errors.Is(err, mail.ErrRejected) or mail.ErrInvalid -> dead-letter
```

Treat queue payloads as secrets (they contain reset links and attachments),
encrypt or expire them, and keep attachments out of the payload by storing a
reference.
