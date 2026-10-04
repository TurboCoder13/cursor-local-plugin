package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

type checkpoint struct {
	History      []string
	State        []byte
	Conversation string
	Updated      time.Time
	Size         int
	Catalog      string
}
type turnQueue struct {
	Lock  chan struct{}
	Count int
}
type cacheCounters struct{ Hits, Misses, Invalidations, Fallbacks, Commits uint64 }

func sessionIdentity(e execution) string {
	if value, ok := e.Metadata["execution_session_id"].(string); ok && value != "" {
		return value
	}
	for _, name := range []string{"X-Session-ID", "Session_id", "X-Client-Request-Id"} {
		for key, values := range e.Headers {
			if strings.EqualFold(name, key) && len(values) > 0 && values[0] != "" {
				return values[0]
			}
		}
	}
	for _, raw := range [][]byte{e.OriginalRequest, e.Payload} {
		var body map[string]any
		if json.Unmarshal(raw, &body) == nil {
			for _, key := range []string{"conversation_id", "session_id"} {
				if value, ok := body[key].(string); ok && value != "" {
					return value
				}
			}
		}
	}
	value, _ := e.Metadata["derived_session_id"].(string)
	return value
}
func sessionKey(e execution, c chatRequest, credential credentials) string {
	session := sessionIdentity(e)
	if session == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(credential.AccountID + "\x00" + c.Model + "\x00" + session))
	return hex.EncodeToString(digest[:])
}
func (s *Service) acquireTurn(ctx context.Context, key string) (func(), error) {
	if key == "" {
		return func() {}, nil
	}
	s.mu.Lock()
	q := s.turns[key]
	if q == nil {
		q = &turnQueue{Lock: make(chan struct{}, 1)}
		s.turns[key] = q
	}
	if q.Count >= 9 || s.waiting >= 64 {
		if q.Count == 0 {
			delete(s.turns, key)
		}
		s.mu.Unlock()
		return nil, reject(409, "session turn queue is full")
	}
	q.Count++
	s.waiting++
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		q.Count--
		s.waiting--
		if q.Count == 0 {
			delete(s.turns, key)
		}
		s.mu.Unlock()
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case q.Lock <- struct{}{}:
		if ctx.Err() != nil {
			<-q.Lock
			release()
			return nil, reject(409, "session wait cancelled")
		}
		return func() { <-q.Lock; release() }, nil
	case <-timer.C:
		release()
		return nil, reject(409, "session turn queue wait exceeded 30 seconds")
	case <-ctx.Done():
		release()
		return nil, reject(409, "session turn queue wait was cancelled")
	}
}
func (s *Service) cached(key string, c chatRequest) (checkpoint, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, v := range s.checkpoints {
		if now.Sub(v.Updated) >= 15*time.Minute {
			delete(s.checkpoints, k)
		}
	}
	cp, exists := s.checkpoints[key]
	if !exists {
		s.cacheStats.Misses++
		return checkpoint{}, "miss"
	}
	if cp.Catalog != catalogKey(c) || len(c.Canonical) <= len(cp.History) || !slices.Equal(c.Canonical[:len(cp.History)], cp.History) {
		delete(s.checkpoints, key)
		s.cacheStats.Invalidations++
		return checkpoint{}, "invalidated"
	}
	s.cacheStats.Hits++
	return cp, "hit"
}
func (s *Service) storeCheckpoint(key string, cp checkpoint) bool {
	if key == "" || len(cp.State) == 0 {
		return false
	}
	cp.Size = len(cp.State)
	for _, line := range cp.History {
		cp.Size += len(line)
	}
	if cp.Size > 16<<20 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.checkpoints, key)
	for {
		total := cp.Size
		oldest := ""
		var timestamp time.Time
		for k, v := range s.checkpoints {
			total += v.Size
			if oldest == "" || v.Updated.Before(timestamp) {
				oldest = k
				timestamp = v.Updated
			}
		}
		if total <= 16<<20 && len(s.checkpoints) < 64 {
			break
		}
		delete(s.checkpoints, oldest)
	}
	s.checkpoints[key] = cp
	s.cacheStats.Commits++
	return true
}
func (s *Service) runCheckpointed(ctx context.Context, e execution, c chatRequest, credential credentials, consume func(event) (bool, error)) error {
	key := sessionKey(e, c, credential)
	release, err := s.acquireTurn(ctx, key)
	if err != nil {
		return err
	}
	defer release()
	historyHasToolResult := false
	full := c
	c.ConversationID = randomID()
	reused := false
	if key != "" {
		cp, reason := s.cached(key, c)
		s.recordCache(e.AuthID, reason)
		if reason == "hit" {
			reused = true
			for _, m := range c.Messages[len(cp.History):] {
				historyHasToolResult = historyHasToolResult || m.Role == "tool"
			}
			c.Checkpoint = append([]byte(nil), cp.State...)
			c.ConversationID = cp.Conversation
			var prompt strings.Builder
			for _, m := range c.Messages[len(cp.History):] {
				decoded, _ := parseContent(m.Content)
				m.Content, _ = json.Marshal(decoded.Text)
				line, _ := json.Marshal(m)
				prompt.Write(line)
				prompt.WriteByte('\n')
			}
			c.Prompt = prompt.String()
			// Attachments are already present in the checkpoint; only suffix attachments travel.
			c.Attachments = nil
			for _, m := range c.Messages[len(cp.History):] {
				decoded, _ := parseContent(m.Content)
				c.Attachments = append(c.Attachments, decoded.Attachments...)
			}
		}
	}
	var state []byte
	var text strings.Builder
	exposed, unsafe, limited := false, false, false
	toolSeen, imageSeen, interactionSeen := false, false, false
	callback := func(ev event) (bool, error) {
		if len(ev.Checkpoint) > 0 {
			state = append([]byte(nil), ev.Checkpoint...)
		}
		if ev.Interaction {
			interactionSeen = true
			unsafe = true
		}
		toolSeen = toolSeen || ev.Call != nil
		imageSeen = imageSeen || ev.Image != ""
		if ev.Thinking || ev.Done {
			exposed = true
		}
		if ev.Call != nil || ev.Image != "" {
			unsafe = true
		}
		if ev.Text != "" || ev.Call != nil || ev.Image != "" {
			exposed = true
			text.WriteString(ev.Text)
		}
		stop, err := consume(ev)
		limited = limited || stop
		return stop, err
	}
	err = s.run(ctx, c, credential.AccessToken, callback)
	if err != nil && reused && !exposed && !unsafe && !historyHasToolResult && ctx.Err() == nil && replayAllowed(err) {
		s.mu.Lock()
		delete(s.checkpoints, key)
		s.cacheStats.Fallbacks++
		s.mu.Unlock()
		s.recordCache(e.AuthID, "fallback")
		state = nil
		full.ConversationID = randomID()
		c = full
		err = s.run(ctx, c, credential.AccessToken, callback)
	}
	if err == nil && !toolSeen && !imageSeen && !limited && text.Len() > 0 && len(state) > 0 {
		m := message{Role: "assistant"}
		m.Content, _ = json.Marshal(text.String())
		raw, _ := json.Marshal(m)
		history := append(slices.Clone(full.Canonical), string(raw))
		committed := s.storeCheckpoint(key, checkpoint{History: history, State: state, Conversation: c.ConversationID, Updated: time.Now(), Catalog: catalogKey(full)})
		if committed {
			s.recordCache(e.AuthID, "commit")
		}
	} else if key != "" {
		s.mu.Lock()
		delete(s.checkpoints, key)
		s.mu.Unlock()
	}
	if err != nil {
		var original *failure
		if errors.As(err, &original) {
			copy := *original
			copy.OutputExposed = exposed
			copy.ToolExposed = toolSeen
			copy.InteractionResponded = interactionSeen
			err = &copy
		}
	}
	return err
}
func replayAllowed(err error) bool {
	f, ok := err.(*failure)
	return ok && f.Replayable && f.HTTPStatus != http.StatusUnauthorized && f.HTTPStatus != http.StatusForbidden && f.HTTPStatus != http.StatusTooManyRequests
}

func catalogKey(c chatRequest) string {
	raw, _ := json.Marshal(c.Tools)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (s *Service) recordCache(id, kind string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	metric := s.cacheUsage[id]
	if metric == nil {
		if len(s.cacheUsage) >= 4096 {
			return
		}
		metric = &cacheCounters{}
		s.cacheUsage[id] = metric
	}
	switch kind {
	case "hit":
		metric.Hits++
	case "miss":
		metric.Misses++
	case "invalidated":
		metric.Invalidations++
	case "fallback":
		metric.Fallbacks++
	case "commit":
		metric.Commits++
	}
}
