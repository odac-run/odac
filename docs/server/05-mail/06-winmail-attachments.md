## 📎 Outlook winmail.dat Attachments
Outlook and Exchange sometimes send mail in Microsoft's own TNEF format. The attachments and the formatted body travel inside a single file named `winmail.dat`, which no mail client other than Outlook can open. The recipient sees a `winmail.dat` attachment and none of the files the sender attached.

ODAC unpacks it on arrival. Before the message reaches a mailbox, the `winmail.dat` part is replaced by the files inside it as ordinary attachments, so any mail client shows them.

### What Changes
- Every file inside `winmail.dat` becomes an attachment with its original name and type.
- Inline images keep their `Content-ID`, so an HTML body that references them still shows them.
- An attached Outlook message becomes an attached `.eml` message, with its own attachments inside it.
- When the message has no readable body outside `winmail.dat`, the body inside it is used. A body that exists only as RTF is attached as `message.rtf`.
- A `winmail.dat` that only repeats a body the message already has is removed.

A converted message carries this header:

```
X-Odac-Tnef: converted
```

Nothing else in the message changes: its headers, its text and its other attachments are kept exactly as they arrived.

### What Stays as It Is
- **Meeting requests, read receipts and other non-mail items.** Their meaning is in Outlook's format, and as plain attachments they would lose it. Outlook users still read them normally.
- **A `winmail.dat` ODAC cannot read completely.** A damaged or truncated file is delivered untouched rather than with part of it missing.
- **Mail your own accounts send.** The copy in the sender's Sent folder and the copy sent to remote recipients are what the sender wrote. Only the copies delivered to mailboxes on this server are converted.

### The Original Is Kept
ODAC stores the message exactly as it arrived next to the converted copy. Mail clients only see the converted one. Because the stored copy is changed, its DKIM signature no longer verifies; this has no effect on delivery, since the message was already checked when it arrived (see the `Authentication-Results` header).

### Turning It Off
Set `ODAC_MAIL_TNEF=off` on the ODAC container to deliver `winmail.dat` as it arrives. It is on by default (`ODAC_MAIL_TNEF=on`).
