## 🛡️ Spam Filtering
ODAC screens every message that arrives without a login before it reaches a mailbox. Mail sent by your own accounts (an authenticated SMTP session) is never screened.

### What Is Checked
- **SPF**: is the sending server allowed to send for the envelope sender's domain?
- **DKIM**: does the message carry a valid signature, and for which domain?
- **DMARC**: does SPF or DKIM pass for the domain in the `From:` header, and what does that domain ask receivers to do when neither does?
- **HELO**: the name the sending server introduces itself with. A bare IP, a name without a dot, or one of your own domains coming from outside all count against the message.
- **Headers**: exactly one `From:` with one address, plus `Date:` and `Message-ID:`.
- **Forged local accounts**: a message from outside whose sender (the envelope sender or the `From:` header) is one of your own mail accounts.
- **Local domain spoofing**: a message claiming any address on one of your own domains (or a subdomain), in the envelope sender or the `From:` header, with no SPF or DKIM pass for that domain.

A DNS lookup that times out or fails is treated as "unknown" and never counts against a message. When a timed-out SPF or DKIM lookup is the only thing standing between a message and a refusal, the message gets a temporary `451` and the sending server retries.

### What Happens to a Message
Each check adds to a score. The result is one of:

- **Inbox**: the score stays below 5.
- **Junk**: the score reaches 5, or the sender's domain publishes DMARC `p=quarantine` and the message fails it.
- **Rejected**: the sender's domain publishes DMARC `p=reject` and the message fails it, the message names one of your accounts as its sender, or it claims one of your domains without passing SPF or DKIM for it. The sending server receives `550 5.7.1` and informs the sender; ODAC sends no bounce itself.

Your accounts always send through ODAC with a login, so a message from outside naming one of them was not sent by it. It is refused even when SPF and DKIM pass. If the account lookup itself fails, the message gets a temporary `451` and the sending server retries.

An address on your domain with no account behind it (`noreply@`, `founder@`) can still be used by a service you set up, such as a newsletter platform or a contact form provider, as long as SPF or DKIM passes for your domain: add the service to the domain's SPF record or publish the DKIM key it gives you. This applies whatever DMARC record the domain has, and even when it has none, so a service that works elsewhere only because the domain publishes no DMARC policy is refused here until it is set up.

Every screened message gets two headers you can inspect in any mail client:

```
Authentication-Results: mx.example.com;
	spf=pass smtp.mailfrom=news@sender.example;
	dkim=pass header.d=sender.example;
	dmarc=pass header.from=sender.example
X-Spam-Status: No, score=-1.0 required=5.0 tests=DMARC_PASS
```

`tests` lists every check that fired, which is the first place to look when a message lands somewhere unexpected.

### Choosing a Mode
Set `ODAC_MAIL_SPAM` on the ODAC container:

| Value | Behaviour |
|---|---|
| `enforce` (default) | Inbox, Junk or reject, as described above |
| `junk` | Never rejects; anything `enforce` would refuse goes to Junk instead |
| `tag` | Adds the headers but always delivers to the inbox |
| `off` | No checks and no headers |

`tag` is useful to watch the filter's decisions for a while before letting it act on them.

> **Note:** a mailing list or a forwarding service resends mail from outside. A message one of your accounts sent to a list comes back to your other members with that account as its sender and is refused, and the same service can break other senders' DMARC alignment. If such mail is being rejected, switch to `junk` so it is kept and can be recovered from the Junk folder.
