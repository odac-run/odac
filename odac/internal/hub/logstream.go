// logstream.go ports Hub.js's app.logs.on/off and app.build_logs.on/off
// commands: buffered log forwarding (flush at 50 entries or 500ms,
// whichever first; flushed on unsubscribe and on disconnect).
package hub

import (
	"sync"
	"time"

	"odac/internal/applog"
	"odac/internal/jscanon"
)

const (
	logBatchSize  = 50
	logFlushDelay = 500 * time.Millisecond
)

// logBatcher is one subscription's buffer + timer, shared by the runtime
// and build log paths (they differ only in message type and payload keys).
type logBatcher struct {
	mu      sync.Mutex
	entries []applog.Entry
	timer   *time.Timer
	send    func(batch []applog.Entry)
}

func (b *logBatcher) push(entry applog.Entry) {
	b.mu.Lock()
	b.entries = append(b.entries, entry)
	if len(b.entries) >= logBatchSize {
		if b.timer != nil {
			b.timer.Stop()
			b.timer = nil
		}
		batch := b.entries
		b.entries = nil
		b.mu.Unlock()
		b.send(batch)
		return
	}
	if b.timer == nil {
		b.timer = time.AfterFunc(logFlushDelay, b.flush)
	}
	b.mu.Unlock()
}

func (b *logBatcher) flush() {
	b.mu.Lock()
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	batch := b.entries
	b.entries = nil
	b.mu.Unlock()
	if len(batch) > 0 {
		b.send(batch)
	}
}

// logsOn ports the app.logs.on command.
func (h *Hub) logsOn(payload any) (any, error) {
	app := str(pmap(payload)["app"])
	h.log.Log("[Hub] Subscription request for app: %s", app)

	h.mu.Lock()
	_, exists := h.logSubs[app]
	h.mu.Unlock()
	if exists {
		return map[string]any{"success": true, "message": "Already subscribed"}, nil
	}
	if h.deps.App == nil {
		return map[string]any{"success": false, "message": "App not running or logs unavailable"}, nil
	}

	batcher := &logBatcher{send: func(batch []applog.Entry) {
		h.sendSignedMessage("log.stream", jscanon.Obj{
			{K: "app", V: app},
			{K: "batch", V: entryList(batch)},
		})
	}}

	unsubscribe := h.deps.App.SubscribeToLogs(app, batcher.push)
	if unsubscribe == nil {
		return map[string]any{"success": false, "message": "App not running or logs unavailable"}, nil
	}

	h.log.Log("[Hub] Successfully subscribed to %s", app)
	h.storeSub(app, func() {
		batcher.flush() // send remaining
		unsubscribe()
	})
	return map[string]any{"success": true, "message": "Subscribed to logs"}, nil
}

// logsOff ports app.logs.off (returns undefined like Node — the
// command.response falls back to {result: true}).
func (h *Hub) logsOff(payload any) (any, error) {
	h.removeSub(str(pmap(payload)["app"]))
	return nil, nil
}

// buildLogsOn ports app.build_logs.on: live build batching, or a one-shot
// replay of the last build log when no build is active.
func (h *Hub) buildLogsOn(payload any) (any, error) {
	app := str(pmap(payload)["app"])
	key := app + ":build"

	h.mu.Lock()
	_, exists := h.logSubs[key]
	h.mu.Unlock()
	if exists {
		return map[string]any{"success": true, "message": "Already subscribed"}, nil
	}

	batcher := &logBatcher{send: func(batch []applog.Entry) {
		h.sendSignedMessage("build.log", jscanon.Obj{
			{K: "app", V: app},
			{K: "batch", V: entryList(batch)},
		})
	}}

	unsubscribe := h.deps.Container.SubscribeToBuildLogs(app, batcher.push)
	if unsubscribe != nil {
		h.storeSub(key, func() {
			batcher.flush()
			unsubscribe()
		})
		return map[string]any{"success": true, "message": "Subscribed to active build logs"}, nil
	}

	// No active build → send the last log.
	content := h.deps.Container.GetLastBuildLog(app)
	h.sendSignedMessage("build.log", jscanon.Obj{
		{K: "app", V: app},
		{K: "content", V: content},
		{K: "finished", V: true},
	})
	return map[string]any{"success": true, "message": "Sent last build log"}, nil
}

// buildLogsOff ports app.build_logs.off.
func (h *Hub) buildLogsOff(payload any) (any, error) {
	h.removeSub(str(pmap(payload)["app"]) + ":build")
	return map[string]any{"success": true, "message": "Unsubscribed from build logs"}, nil
}

func (h *Hub) storeSub(key string, unsubscribe func()) {
	h.mu.Lock()
	h.logSubs[key] = unsubscribe
	h.mu.Unlock()
}

func (h *Hub) removeSub(key string) {
	h.mu.Lock()
	unsub := h.logSubs[key]
	delete(h.logSubs, key)
	h.mu.Unlock()
	if unsub != nil {
		unsub()
	}
}

// unsubscribeAllLogs ports #unsubscribeAllLogs (disconnect/stop cleanup).
func (h *Hub) unsubscribeAllLogs() {
	h.mu.Lock()
	if len(h.logSubs) == 0 {
		h.mu.Unlock()
		return
	}
	count := len(h.logSubs)
	subs := h.logSubs
	h.logSubs = map[string]func(){}
	h.mu.Unlock()

	h.log.Log("[Hub] Clearing %s active log subscriptions", count)
	for _, unsub := range subs {
		unsub()
	}
}

func entryList(batch []applog.Entry) []any {
	list := make([]any, len(batch))
	for i, e := range batch {
		list[i] = entryTree(e)
	}
	return list
}
