package config

import (
	"log"
	"os"
	"strconv"
	"sync"
)

// defaultMaxMessageBytes is the largest message accepted over SMTP. The old
// 10MB ceiling rejected ordinary mail: base64 inflates an attachment by a
// third, so a 25MB attachment (what Gmail and Outlook allow) arrives as a
// message of roughly 34MB.
const defaultMaxMessageBytes int64 = 35 * 1024 * 1024

// minMessageBytes keeps a misconfigured environment from setting a ceiling so
// low that plain text mail bounces.
const minMessageBytes int64 = 64 * 1024

var (
	maxMessageOnce  sync.Once
	maxMessageBytes int64
)

// MaxMessageBytes returns the SMTP message size ceiling, overridable with
// ODAC_MAIL_MAX_MESSAGE_BYTES. The value is read once at first use so every
// listener and session agrees on the same limit for the process lifetime.
func MaxMessageBytes() int64 {
	maxMessageOnce.Do(func() {
		maxMessageBytes = defaultMaxMessageBytes
		raw := os.Getenv("ODAC_MAIL_MAX_MESSAGE_BYTES")
		if raw == "" {
			return
		}
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || v < minMessageBytes {
			log.Printf("[Mail] Ignoring invalid ODAC_MAIL_MAX_MESSAGE_BYTES=%q (minimum %d)", raw, minMessageBytes)
			return
		}
		maxMessageBytes = v
		log.Printf("[Mail] Message size limit set to %d bytes", v)
	})
	return maxMessageBytes
}
