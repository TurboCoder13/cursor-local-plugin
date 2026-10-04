package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func requestChat(t *testing.T, chat any, session string) (execution, chatRequest, credentials) {
	t.Helper()
	var request execution
	raw := envelope(t, chat)
	if json.Unmarshal(raw, &request) != nil {
		t.Fatal("envelope")
	}
	request.Headers = http.Header{"X-Session-Id": {session}}
	e, c, credential, err := parseExecution(marshal(t, request))
	if err != nil {
		t.Fatal(err)
	}
	return e, c, credential
}
func imageData() []byte { return []byte{137, 80, 78, 71, 13, 10, 26, 10, 0, 0, 0, 13, 73, 72, 68, 82} }

func TestContentAndToolChoiceParity(t *testing.T) {
	image := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageData())
	chat := basicChat()
	chat["messages"] = []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "input_text", "text": "look"}, map[string]any{"type": "input_image", "image_url": image},
		map[string]any{"type": "input_file", "filename": "../note.txt", "file_data": base64.StdEncoding.EncodeToString([]byte("hello"))},
	}}}
	_, c, _, err := parseExecution(envelope(t, chat))
	if err != nil || len(c.Attachments) != 2 || c.Attachments[1].Name != "note.txt" || c.Attachments[1].Text != "hello" {
		t.Fatalf("attachments: %v", err)
	}
	c.ConversationID = "conversation"
	packet, err := runPacket(c)
	if err != nil || !bytes.Contains(packet, imageData()) || !bytes.Contains(packet, []byte("note.txt")) {
		t.Fatal("selected context omitted attachments")
	}
	for _, choice := range []any{"auto", "required", map[string]any{"type": "function", "function": map[string]string{"name": "b"}}} {
		tools := basicChat()
		tools["tools"] = []tool{{Type: "function", Function: function{Name: "a"}}, {Type: "function", Function: function{Name: "b"}}}
		tools["tool_choice"] = choice
		_, parsed, _, err := parseExecution(envelope(t, tools))
		if err != nil {
			t.Fatal(err)
		}
		if _, forced := choice.(map[string]any); forced && (len(parsed.Tools) != 1 || parsed.Tools[0].Function.Name != "b") {
			t.Fatal("forced tool catalog not filtered")
		}
	}
	for _, raw := range []string{`[{"type":"input_file","file_id":"unresolved"}]`, `[{"type":"image_url","image_url":"http://localhost/private"}]`, `[{"type":"file","file":{"file_data":"data:application/octet-stream;base64,/w=="}}]`} {
		if _, err := parseContent(json.RawMessage(raw)); err == nil {
			t.Fatal("unresolvable or binary content accepted")
		}
	}
}

func toolPacket(id, name string) []byte {
	return bytesField(2, fields(numberField(1, 11), textField(15, "execution"), bytesField(11, fields(textField(3, id), textField(4, clientToolProvider), textField(5, name)))))
}
func TestMultipleToolsAndStableIDs(t *testing.T) {
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _, _ = readFrame(r.Body)
		w.Header().Set("Content-Type", "application/connect+proto")
		_ = writeFrame(w, toolPacket("original_a", "read"))
		_ = writeFrame(w, toolPacket(strings.Repeat("long", 40), "read"))
		_ = writeFrame(w, donePacket())
		w.(http.Flusher).Flush()
	})
	chat := basicChat()
	chat["tools"] = []tool{{Type: "function", Function: function{Name: "read"}}}
	e, c, credential := requestChat(t, chat, "")
	result, err := s.execute(context.Background(), e, c, credential)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Choices []struct {
			Message struct {
				ToolCalls []toolCall `json:"tool_calls"`
			}
		}
	}
	_ = json.Unmarshal(result.(map[string]any)["Payload"].([]byte), &response)
	calls := response.Choices[0].Message.ToolCalls
	if len(calls) != 2 || calls[0].ID != "original_a" || len(calls[1].ID) > 64 {
		t.Fatalf("multi-tool output: %+v", calls)
	}
	if stableToolID("a\n") != stableToolID("a\n") || strings.Contains(stableToolID("a\n"), "\n") {
		t.Fatal("unstable or unsafe tool id")
	}
	p := protocolState{}
	_, _ = p.receive(toolPacket("duplicate", "read"), c)
	ev, err := p.receive(toolPacket("duplicate", "read"), c)
	if err != nil || ev.Call != nil {
		t.Fatal("duplicate tool exposure")
	}
	a := answer{reason: "stop", limit: 1}
	_, err = a.accept(event{Call: &calls[0]})
	if err != nil || len(a.calls) != 0 || a.reason != "length" {
		t.Fatal("partial oversized tool was exposed")
	}
}

func generatedPacket() []byte {
	success := fields(textField(1, "/workspace/assets/image.png"), textField(2, base64.StdEncoding.EncodeToString(imageData())))
	imageCall := bytesField(2, bytesField(1, success))
	completed := fields(textField(1, "generated"), bytesField(2, bytesField(28, imageCall)))
	return bytesField(1, bytesField(3, completed))
}
func TestImageGenerationAndVirtualWrites(t *testing.T) {
	query := fields(numberField(1, 17), bytesField(12, bytesField(1, textField(1, "A small bird"))))
	reply, err := interactionRefusal(query)
	if err != nil || !bytes.Contains(reply, []byte("A small bird")) {
		t.Fatal("native image approval failed")
	}
	outer, _ := decodeWire(reply)
	response, _ := decodeWire(outer.data(6))
	if response.num(1) != 17 {
		t.Fatal("image query lost correlation")
	}
	p := protocolState{}
	ev, err := p.receive(generatedPacket(), chatRequest{})
	if err != nil || !strings.HasPrefix(ev.Image, "data:image/png;base64,") {
		t.Fatal("image completion failed")
	}
	for _, location := range []string{"/etc/private.png", "/workspace/assets/../private.png", "/workspace/assets/nested/image.png"} {
		write := fields(numberField(1, 12), textField(15, "write"), bytesField(3, fields(textField(1, location), bytesField(5, imageData()))))
		ev, err = p.imageWrite(mustWire(t, write))
		if err != nil || !bytes.Contains(ev.Replies[0], []byte(nativePolicy)) || len(p.imageFiles) != 0 {
			t.Fatal("image write escaped private workspace")
		}
	}
	for i := 0; i < 2; i++ {
		data := append(imageData(), byte(i))
		write := fields(numberField(1, 12), bytesField(3, fields(textField(1, "/workspace/assets/image.png"), bytesField(5, data))))
		ev, err = p.imageWrite(mustWire(t, write))
		if err != nil || len(ev.Replies) != 1 {
			t.Fatal("valid virtual image write failed")
		}
	}
	if len(p.imageFiles) != 2 {
		t.Fatal("different images overwrote same path")
	}
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		_, _, _ = readFrame(r.Body)
		w.Header().Set("Content-Type", "application/connect+proto")
		_ = writeFrame(w, generatedPacket())
		_ = writeFrame(w, donePacket())
		w.(http.Flusher).Flush()
	})
	e, c, credential := requestChat(t, basicChat(), "")
	result, err := s.execute(context.Background(), e, c, credential)
	if err != nil || !bytes.Contains(result.(map[string]any)["Payload"].([]byte), []byte(`"images"`)) {
		t.Fatal("image-only completion rejected")
	}
}
func mustWire(t *testing.T, raw []byte) wire {
	t.Helper()
	w, err := decodeWire(raw)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestNativeRefusalSequenceAndBlobAtomicity(t *testing.T) {
	exec := fields(numberField(1, 77), textField(15, "shell-correlation"), bytesField(14, fields(textField(1, "rm -rf /"), textField(2, "/private"))))
	p := protocolState{}
	ev, err := p.receive(bytesField(2, exec), chatRequest{})
	if err != nil || len(ev.Replies) != 5 {
		t.Fatalf("shell stream sequence: %v", err)
	}
	for i, reply := range ev.Replies {
		w := mustWire(t, reply)
		if i < 4 {
			client := mustWire(t, w.data(2))
			if client.num(1) != 77 || string(client.data(15)) != "shell-correlation" {
				t.Fatal("shell refusal correlation")
			}
		} else {
			control := mustWire(t, w.data(5))
			close := mustWire(t, control.data(1))
			if close.num(1) != 77 {
				t.Fatal("shell close correlation")
			}
		}
	}
	store := func(key string, data []byte) error {
		_, err := p.blob(fields(numberField(1, 1), bytesField(3, fields(textField(1, key), bytesField(2, data)))))
		return err
	}
	if err := store("key", []byte("first")); err != nil {
		t.Fatal(err)
	}
	before := p.bytes
	if err := store("key", make([]byte, (16<<20)+1)); err == nil || p.bytes != before || string(p.blobs["key"]) != "first" {
		t.Fatal("rejected overwrite changed blob accounting")
	}
	if err := store("key", []byte("x")); err != nil || p.bytes != len("key")+1 {
		t.Fatal("overwrite did not reclaim budget")
	}
}

type observedRun struct {
	Prompt       string
	Checkpoint   []byte
	Conversation string
}

func observeRun(t *testing.T, r *http.Request) observedRun {
	t.Helper()
	_, raw, err := readFrame(r.Body)
	if err != nil {
		t.Error(err)
		return observedRun{}
	}
	client := mustWire(t, raw)
	run := mustWire(t, client.data(1))
	action := mustWire(t, run.data(2))
	userAction := mustWire(t, action.data(1))
	user := mustWire(t, userAction.data(1))
	return observedRun{Prompt: string(user.data(1)), Checkpoint: run.data(1), Conversation: string(run.data(5))}
}
func TestCheckpointReuseAndInvalidation(t *testing.T) {
	var mu sync.Mutex
	var observed []observedRun
	state := textField(1, "opaque-checkpoint")
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		run := observeRun(t, r)
		mu.Lock()
		observed = append(observed, run)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/connect+proto")
		_ = writeFrame(w, textPacket("answer"))
		_ = writeFrame(w, donePacket())
		_ = writeFrame(w, bytesField(3, state))
		w.(http.Flusher).Flush()
	})
	chat := basicChat()
	e, c, credential := requestChat(t, chat, "session")
	if _, err := s.execute(context.Background(), e, c, credential); err != nil {
		t.Fatal(err)
	}
	chat["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "text", "text": "hello"}}}, map[string]string{"role": "assistant", "content": "answer"}, map[string]string{"role": "user", "content": "continue"}}
	e, c, credential = requestChat(t, chat, "session")
	if _, err := s.execute(context.Background(), e, c, credential); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(observed) != 2 || !bytes.Equal(observed[1].Checkpoint, state) || strings.Contains(observed[1].Prompt, "hello") || !strings.Contains(observed[1].Prompt, "continue") || observed[0].Conversation != observed[1].Conversation {
		t.Fatalf("checkpoint suffix failed: %+v", observed)
	}
	mu.Unlock()
	// Same history on another account or session must never reuse the checkpoint.
	for _, change := range []string{"account", "session", "edit", "expire"} {
		e, c, credential = requestChat(t, chat, "session")
		switch change {
		case "account":
			credential.AccountID = "different"
		case "session":
			e.Headers.Set("X-Session-Id", "other")
		case "edit":
			c.Canonical[0] += "edit"
		case "expire":
			key := sessionKey(e, c, credential)
			s.mu.Lock()
			cp := s.checkpoints[key]
			cp.Updated = time.Now().Add(-16 * time.Minute)
			s.checkpoints[key] = cp
			s.mu.Unlock()
		}
		if _, err := s.execute(context.Background(), e, c, credential); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		last := observed[len(observed)-1]
		mu.Unlock()
		if len(last.Checkpoint) != 0 {
			t.Fatalf("checkpoint leaked across %s", change)
		}
	}
}
func TestCheckpointFallbackOnlyBeforeExposure(t *testing.T) {
	for _, kind := range []string{"safe", "text", "interaction"} {
		t.Run(kind, func(t *testing.T) {
			var mu sync.Mutex
			attempts := 0
			s := testService(t, func(w http.ResponseWriter, r *http.Request) {
				_ = observeRun(t, r)
				mu.Lock()
				attempts++
				attempt := attempts
				mu.Unlock()
				w.Header().Set("Content-Type", "application/connect+proto")
				if attempt == 1 {
					if kind == "text" {
						_ = writeFrame(w, textPacket("already exposed"))
					}
					if kind == "interaction" {
						_ = writeFrame(w, bytesField(7, fields(numberField(1, 1), bytesField(2, nil))))
						w.(http.Flusher).Flush()
						_, _, _ = readFrame(r.Body)
					}
					if kind == "safe" {
						frame := []byte(`{"error":{"code":"internal"}}`)
						header := []byte{2, 0, 0, 0, byte(len(frame))}
						_, _ = io.Copy(w, bytes.NewReader(append(header, frame...)))
					} else {
						_ = writeFrame(w, []byte{0xff})
					}
					w.(http.Flusher).Flush()
					return
				}
				_ = writeFrame(w, textPacket("replayed"))
				_ = writeFrame(w, donePacket())
				w.(http.Flusher).Flush()
			})
			chat := basicChat()
			chat["messages"] = append(chat["messages"].([]any), map[string]string{"role": "user", "content": "next"})
			e, c, credential := requestChat(t, chat, "fallback")
			key := sessionKey(e, c, credential)
			s.storeCheckpoint(key, checkpoint{History: c.Canonical[:1], State: textField(1, "state"), Conversation: "cached", Updated: time.Now(), Catalog: catalogKey(c)})
			_, err := s.execute(context.Background(), e, c, credential)
			mu.Lock()
			count := attempts
			mu.Unlock()
			if kind == "safe" {
				if err != nil || count != 2 {
					t.Fatalf("safe replay: attempts=%d err=%v", count, err)
				}
			} else if err == nil || count != 1 {
				t.Fatalf("replayed after %s exposure", kind)
			}
		})
	}
}

func TestSessionQueueLimitsAndCancellation(t *testing.T) {
	s := NewService(nil)
	defer s.Shutdown()
	release, err := s.acquireTurn(context.Background(), "same")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.acquireTurn(ctx, "same")
			if err == nil {
				t.Error("cancelled follower acquired session")
			}
		}()
	}
	deadline := time.Now().Add(time.Second)
	for {
		s.mu.Lock()
		count := s.waiting
		s.mu.Unlock()
		if count == 9 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("followers not admitted")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := s.acquireTurn(context.Background(), "same"); err == nil {
		t.Fatal("ninth follower admitted")
	}
	cancel()
	wg.Wait()
	s.mu.Lock()
	if s.waiting != 1 {
		t.Error("cancelled followers retained references")
	}
	s.mu.Unlock()
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if unlock, err := s.acquireTurn(cancelled, "different"); err == nil {
		unlock()
		t.Fatal("already cancelled turn admitted")
	}
}

func TestCapacityDisabledModelsAndOptionalFailure(t *testing.T) {
	for _, optional := range []bool{true, false} {
		t.Run(map[bool]string{true: "available", false: "unavailable"}[optional], func(t *testing.T) {
			s := testService(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Content-Type-Options", "nosniff")
				if strings.Contains(r.URL.Path, "AvailableModels") {
					w.Header().Set("Content-Type", "application/json")
					if !optional {
						w.WriteHeader(503)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]any{"name": "model-a", "serverModelName": "a", "contextTokenLimit": 2000000, "supportsNonMaxMode": true}, map[string]any{"name": "b", "contextTokenLimit": 300, "supportsNonMaxMode": false}}})
					return
				}
				w.Header().Set("Content-Type", "application/proto")
				_, _ = io.Copy(w, bytes.NewReader(fields(bytesField(1, textField(1, "a")), bytesField(1, textField(1, "b")))))
			})
			credential := fixtureCredentials()
			credential.DisabledModels = []string{"b"}
			result, err := s.models(context.Background(), marshal(t, map[string]any{"StorageJSON": marshal(t, credential)}))
			if err != nil {
				t.Fatal(err)
			}
			models := result.(map[string]any)["Models"].([]any)
			if len(models) != 1 {
				t.Fatal("disabled discovery model visible")
			}
			m := models[0].(map[string]any)
			if optional && (m["NativeContextLength"] != int64(2000000) || m["ContextLength"] != int64(1000000)) {
				t.Fatal("native/policy capacities conflated")
			}
			if !optional {
				if _, exists := m["ContextLength"]; exists {
					t.Fatal("invented native capacity")
				}
			}
			chat := basicChat()
			chat["model"] = "cursor/b"
			if _, _, _, err := parseExecution(marshal(t, execution{Payload: marshal(t, chat), StorageJSON: marshal(t, credential)})); err == nil {
				t.Fatal("disabled execution accepted")
			}
		})
	}
}

func TestToolProgressGuardAndCountTokens(t *testing.T) {
	var messages []message
	for i, id := range []string{"a", "b", "c"} {
		args := `{"a":1,"b":2}`
		if i == 1 {
			args = `{ "b":2, "a":1 }`
		}
		messages = append(messages, message{Role: "assistant", ToolCalls: []toolCall{{ID: id, Function: function{Name: "read", Arguments: args}}}}, message{Role: "tool", ToolCallID: id, Content: json.RawMessage(`"same"`)})
	}
	if validateProgress(messages, []string{"read"}) == nil {
		t.Fatal("no-progress guard missed identical linked exchanges")
	}
	if validateProgress(messages, nil) != nil {
		t.Fatal("guard enabled by default")
	}
	messages[2] = message{Role: "user", Content: json.RawMessage(`"new request"`)}
	if validateProgress(messages, []string{"read"}) != nil {
		t.Fatal("new user message did not break guard")
	}
	s := NewService(nil)
	defer s.Shutdown()
	result, err := s.countTokens(envelope(t, basicChat()))
	if err != nil || !bytes.Contains(result.(map[string]any)["Payload"].([]byte), []byte(`"estimated":true`)) {
		t.Fatal("counting contract failed")
	}
}

func TestLifecycleDeduplicationAndWindows(t *testing.T) {
	s := NewService(nil)
	defer s.Shutdown()
	id := ID + "-account"
	s.recordExecution(id, 5, 7)
	for i := 0; i < 2; i++ {
		_, err := s.lifecycle("request.complete", marshal(t, map[string]any{"RequestID": "logical", "Outcome": "succeeded", "Metadata": map[string]string{"selected_auth_id": id}}))
		if err != nil {
			t.Fatal(err)
		}
	}
	u := s.usage[id]
	if u.Success != 1 || u.Failed != 0 || u.Executions != 1 || u.PromptTokens != 5 {
		t.Fatalf("retry accounting: %+v", u)
	}
	now := time.Now()
	f := completionFilter{}
	if f.seen("x", now) || !f.seen("x", now.Add(15*time.Minute)) {
		t.Fatal("dedup retention broke at rotation")
	}
	if f.seen("x", now.Add(31*time.Minute)) {
		t.Fatal("dedup windows never expire")
	}
	if len(f.Windows[0])+len(f.Windows[1]) != 16<<20 {
		t.Fatal("dedup storage budget")
	}
}

func TestManagementStorageFilteringAndPreservation(t *testing.T) {
	credential := fixtureCredentials()
	credential.Email = "account@example.test"
	credential.ToolLoopGuardTools = []string{"read"}
	stored := map[string]any{"type": credential.Type, "access_token": credential.AccessToken, "refresh_token": credential.RefreshToken, "account_id": credential.AccountID, "email": credential.Email, "expires_at": credential.Expires, "tool_loop_guard_tools": credential.ToolLoopGuardTools, "custom": "preserve me"}
	saved := false
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if strings.Contains(r.URL.Path, "AvailableModels") {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/proto")
		_, _ = io.Copy(w, bytes.NewReader(bytesField(1, textField(1, "a"))))
	})
	s.host = func(method string, value any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return marshal(t, map[string]any{"files": []managedFile{{ID: ID + "-a", Index: "a", Name: "a.json", Path: "/auth/a.json", Provider: ID, Source: "file"}, {ID: ID + "-projection", Index: "projection", Name: "a.json", Path: "/auth/a.json", Provider: ID, Source: "file"}, {Index: "runtime", Name: "runtime.json", Provider: ID, RuntimeOnly: true}, {Index: "other", Name: "other.json", Provider: "codex"}}}), nil
		case "host.auth.get":
			return marshal(t, map[string]any{"name": "a.json", "json": stored}), nil
		case "host.auth.save":
			body := value.(map[string]any)
			var changed map[string]any
			_ = json.Unmarshal(body["json"].(json.RawMessage), &changed)
			if changed["custom"] != "preserve me" || changed["access_token"] != credential.AccessToken {
				t.Error("model control overwrote credentials")
			}
			saved = true
			return json.RawMessage(`{}`), nil
		default:
			return nil, errors.New("unexpected host callback")
		}
	}
	files, err := s.accountFiles()
	if err != nil || len(files) != 1 {
		t.Fatal("physical/runtime account filtering failed")
	}
	result, err := s.management(marshal(t, map[string]any{"Method": "GET", "Path": "/v0/management" + managementRoot + "/status"}))
	if err != nil {
		t.Fatal(err)
	}
	response := result.(managementResponse)
	if response.StatusCode != 200 || bytes.Contains(response.Body, []byte(credential.AccessToken)) || bytes.Contains(response.Body, []byte("custom")) {
		t.Fatal("status failed or leaked credentials")
	}
	update := map[string]any{"auth_index": "a", "disabled_models": []string{"a"}}
	result, err = s.saveDisabled(context.Background(), marshal(t, update))
	if err != nil || result.(managementResponse).StatusCode != 400 || saved {
		t.Fatal("all-disable confirmation missing")
	}
	update["confirm_disable_all"] = true
	result, err = s.saveDisabled(context.Background(), marshal(t, update))
	if err != nil || result.(managementResponse).StatusCode != 200 || !saved {
		t.Fatal("model save failed")
	}
	result, err = s.management(marshal(t, map[string]any{"Method": "GET", "Path": resourceRoot + "/status"}))
	if err != nil || bytes.Contains(result.(managementResponse).Body, []byte(credential.AccessToken)) {
		t.Fatal("public resource contains account data")
	}
}

func TestLogicalAccountGroupsAreTransitive(t *testing.T) {
	records := []accountRecord{{File: managedFile{ID: "a"}, Credential: credentials{AccountID: "one", Email: "first@example.test"}}, {File: managedFile{ID: "b"}, Credential: credentials{AccountID: "two", Email: "second@example.test"}}, {File: managedFile{ID: "bridge"}, Credential: credentials{AccountID: "one", Email: "second@example.test"}}}
	groups := groupAccounts(records)
	if len(groups) != 1 || len(groups[0].Records) != 3 {
		t.Fatal("identity bridge did not deduplicate logical accounts")
	}
}

func TestRefreshPreservesAccountControls(t *testing.T) {
	claims := marshal(t, map[string]any{"sub": "test-user", "exp": time.Now().Add(time.Hour).Unix()})
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	s := testService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": token})
	})
	c := fixtureCredentials()
	c.DisabledModels = []string{"a"}
	c.ToolLoopGuardTools = []string{"read"}
	result, err := s.refresh(context.Background(), marshal(t, map[string]any{"StorageJSON": marshal(t, c)}))
	if err != nil {
		t.Fatal(err)
	}
	auth := result.(map[string]any)["Auth"].(map[string]any)
	refreshed, err := parseCredentials(auth["StorageJSON"].([]byte))
	if err != nil || len(refreshed.DisabledModels) != 1 || refreshed.DisabledModels[0] != "a" || len(refreshed.ToolLoopGuardTools) != 1 || refreshed.AccountID != c.AccountID {
		t.Fatal("refresh lost persisted controls or identity")
	}
}

func TestGlobalSessionCapacityAndCacheBudgets(t *testing.T) {
	s := NewService(nil)
	defer s.Shutdown()
	var releases []func()
	for i := 0; i < 64; i++ {
		release, err := s.acquireTurn(context.Background(), randomID())
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, err := s.acquireTurn(context.Background(), "overflow"); err == nil {
		t.Fatal("global session admission exceeded 64")
	}
	for _, release := range releases {
		release()
	}
	for i := 0; i < 65; i++ {
		s.storeCheckpoint(randomID(), checkpoint{State: []byte("state"), Updated: time.Now()})
	}
	if len(s.checkpoints) != 64 {
		t.Fatal("checkpoint entry bound")
	}
	s.storeCheckpoint("oversized", checkpoint{State: make([]byte, (16<<20)+1), Updated: time.Now()})
	if _, exists := s.checkpoints["oversized"]; exists {
		t.Fatal("oversized checkpoint cached")
	}
	s.storeCheckpoint("large", checkpoint{State: make([]byte, 16<<20), Updated: time.Now()})
	if len(s.checkpoints) != 1 {
		t.Fatal("checkpoint byte budget was not enforced")
	}
}

func TestChangingToolImagesCountAsProgress(t *testing.T) {
	var history []message
	for i, id := range []string{"one", "two", "three"} {
		source := "data:image/png;base64," + base64.StdEncoding.EncodeToString(append(imageData(), byte(i)))
		history = append(history, message{Role: "assistant", ToolCalls: []toolCall{{ID: id, Function: function{Name: "capture", Arguments: "{}"}}}}, message{Role: "tool", ToolCallID: id, Content: marshal(t, []any{map[string]any{"type": "image_url", "image_url": source}})})
	}
	if validateProgress(history, []string{"capture"}) != nil {
		t.Fatal("changing images were treated as identical tool results")
	}
}
