# Mail

Forge's `mail` package sends email. Messages are validated data, delivery goes
through a `Sender` port, and the defaults are the safe ones. The design is in
[ADR 0024](adr/0024-mail.md).

## Configuration

| Variable | Default | Meaning |
|----------|---------|---------|
| `FORGE_MAIL_URL` | empty (mail disabled) | `smtp://user:password@host:587` (STARTTLS), `smtps://user:password@host:465` (implicit TLS) or `file:///path` (development `.eml` files). Percent-encode `@`, `:` and `/` in the password. |
| `FORGE_MAIL_FROM` | required with a URL | Default sender, `app@example.com` or `App <app@example.com>`. |
| `FORGE_MAIL_ALLOW_PLAINTEXT` | `false` | `true` lets the SMTP sender continue when the server offers no STARTTLS. It never allows the password in clear: AUTH without TLS is sent only to `localhost`, 127.0.0.0/8 or `::1` (a local Mailpit); to any other host the send fails. In `production` it cannot be combined with credentials for a remote host; without credentials it is accepted but logs a warning at start-up, because nothing then stops a network attacker from stripping `STARTTLS` (the mail itself travels in clear). |
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

Header text is encoded so that it reads back exactly as written: a non-ASCII
display name is base64 encoded words (Q-encoding would leave a `\` or `"`
raw, which an address header does not allow); a subject or name containing
`=?` (which a client would decode as a hand-written encoded word) or a run of
spaces is base64 encoded words of the text without its outer spaces (folding
would otherwise collapse the run); other non-ASCII subjects are Q-encoded.
An attachment's media type is at most 127 bytes, so a part header never
approaches the 998-byte line limit.

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

The SMTP sender reads the `AUTH` mechanisms from both `250-AUTH PLAIN LOGIN`
and the legacy `250-AUTH=PLAIN LOGIN` advertisement, prefers `PLAIN` and falls
back to `LOGIN`; a server offering neither fails with `mail.ErrAuthFailed`.
`net/smtp` does not list the extensions it saw, so the legacy form is
recognised when its first mechanism is one of `PLAIN`, `LOGIN`, `CRAM-MD5`,
`XOAUTH2`, `OAUTHBEARER`, `SCRAM-SHA-1`, `SCRAM-SHA-256`, `GSSAPI`, `NTLM` or
`DIGEST-MD5` (the rest of the line is read whatever it holds); a legacy line
starting with any other mechanism is ignored. The extension names are
matched as the server sends them (`AUTH`, upper case, as every common server
does): `net/smtp` does not fold their case.

**A delivery can be duplicated.** SMTP has no way to confirm a message that
was accepted: if the deadline (`FORGE_MAIL_TIMEOUT`) expires, or the
connection drops, after the server accepted the message data but before its
`250` reply reaches Forge, `Send` fails with `mail.ErrUnavailable` although
the server queued the message. A retry then delivers a second copy. This is
inherent to SMTP; make mail handlers tolerate a repeated message (an
idempotent action, or a deduplication key in the body), and do not retry
errors that are not retryable.

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

## Generating a mailer

```sh
forge generate mailer user welcome:name,url password_reset:name,reset_url
```

writes `app/mailers/user_mailer.go` (a typed `UserMailer` with a method and a
data struct per email), its templates under `app/mailers/templates/user/`,
previews and tests, and wires the sender and the previews at `/_mail/user/`
(development only, with `FORGE_MAIL_PREVIEW=true`) into `app/bootstrap/routes_gen.go`
([Generators](GENERATORS.md#mailer)).

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
becomes an `.eml` file (mode 0600) and only its path is logged. The directory
is created 0700; an existing one that is group- or world-accessible is
tightened to 0700 (with a warning), and one that cannot be tightened is an
error. A sticky shared directory such as `/tmp`, and a symbolic link, are left
alone with a warning; the check and the change use one open handle, opened
with `O_NOFOLLOW|O_DIRECTORY` on Linux, macOS, FreeBSD, NetBSD and OpenBSD, so
the path cannot be swapped in between and a FIFO put in its place fails to
open (other platforms open it normally and rely on the `Lstat`/`SameFile`
check). Symbolic links
in the parent components of the path are still followed, and swapping needs
write access to the parent: this is a development-only path.
For a browser
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
would not be sent. HTML is served under a Content-Security-Policy that allows
images only from the preview itself and `data:` URIs (`img-src 'self' data:`):
remote images and fonts are not loaded, so opening a preview does not contact
any server. Inline `cid:` images work.

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
builds (SMTP, run off the request path by the recovery queue).

`recoverymail.Sender` implements `auth.RecoverySender`. The recovery service
calls it after the request was answered, from a bounded queue
(`auth.RecoveryQueue`, generated applications) or the persistent job queue
(`auth/recoveryqueue`), so SMTP latency does not reveal whether an account
exists and a slow mail server does not slow the page; a queue-backed
`mail.Sender` is still useful for retrying delivery (ADR 0013).

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
