package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func fixtureCredentials() credentials {
	return credentials{Type: ID, AccessToken: "test-access-token", RefreshToken: "test-refresh-token", AccountID: "test-user", Expires: time.Now().Add(time.Hour)}
}

func marshal(t *testing.T, value any) []byte {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func envelope(t *testing.T, chat any) []byte {
	t.Helper()
	return marshal(t, execution{Payload: marshal(t, chat), StorageJSON: marshal(t, fixtureCredentials())})
}

func basicChat() map[string]any {
	return map[string]any{"model": "cursor/test-model", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
}

func testService(t *testing.T, handler http.HandlerFunc) *Service {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.StartTLS()
	s := NewService(nil)
	s.base = server.URL
	s.client = server.Client()
	t.Cleanup(func() { s.Shutdown(); server.Close() })
	return s
}

func textPacket(text string) []byte { return bytesField(1, bytesField(1, textField(1, text))) }
func donePacket() []byte            { return bytesField(1, bytesField(14, nil)) }

func TestInputValidation(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"image URL": func(c map[string]any) {
			c["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://127.0.0.1/private"}}}}}
		},
		"unknown role": func(c map[string]any) { c["messages"] = []any{map[string]any{"role": "admin", "content": "hello"}} },
		"orphan tool result": func(c map[string]any) {
			c["messages"] = append(c["messages"].([]any), map[string]any{"role": "tool", "tool_call_id": "unknown", "content": "done"})
		},
		"required tool":   func(c map[string]any) { c["tool_choice"] = "required" },
		"negative budget": func(c map[string]any) { c["max_tokens"] = -1 },
		"JSON output":     func(c map[string]any) { c["response_format"] = map[string]any{"type": "json_schema"} },
		"duplicate tools": func(c map[string]any) {
			c["tools"] = []tool{{Type: "function", Function: function{Name: "read"}}, {Type: "function", Function: function{Name: "read"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			chat := basicChat()
			mutate(chat)
			if _, _, _, err := parseExecution(envelope(t, chat)); err == nil {
				t.Fatal("invalid request was accepted")
			}
		})
	}
	chat := basicChat()
	chat["tools"] = []tool{{Type: "function", Function: function{Name: "read"}}}
	chat["tool_choice"] = "none"
	_, c, _, err := parseExecution(envelope(t, chat))
	if err != nil || len(c.Tools) != 0 {
		t.Fatalf("tool_choice none: %+v, %v", c, err)
	}
}

func TestCredentialOwnershipAndSecretErrors(t *testing.T) {
	s := NewService(nil)
	defer s.Shutdown()
	for _, provider := range []string{"codex", "claude", "cursor"} {
		c := fixtureCredentials()
		c.Type = provider
		response, ok := s.Call("auth.parse", marshal(t, map[string]any{"RawJSON": marshal(t, c)}))
		if !ok || !bytes.Contains(response, []byte(`"Handled":false`)) {
			t.Fatalf("claimed another provider's credentials: %s", response)
		}
	}
	body, ok := encode(nil, errors.New("secret-token-must-not-appear"))
	if ok || bytes.Contains(body, []byte("secret-token")) {
		t.Fatal("raw error leaked a secret")
	}
	if _, ok := s.Call("executor.execute", []byte(`{"Payload":"!bad-base64!"}`)); ok {
		t.Fatal("bad executor envelope accepted")
	}
	if _, ok := s.Call("auth.parse", make([]byte, MaxEnvelopeBytes+1)); ok {
		t.Fatal("oversized envelope accepted")
	}
}

func TestHTTP2ConversationAndToolContinuation(t *testing.T) {
	var mu sync.Mutex
	var prompts []string
	s := testService(t, func(w http.ResponseWriter, req *http.Request) {
		if req.ProtoMajor != 2 || req.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Error("expected HTTP/2 and selected Cursor credential")
		}
		flag, packet, err := readFrame(req.Body)
		if err != nil || flag != 0 {
			t.Error("invalid initial Connect frame")
			return
		}
		client, _ := decodeWire(packet)
		run, _ := decodeWire(client.data(1))
		if run.num(12) != 0 {
			t.Error("run enabled a workspace mode rejected by the live account")
			return
		}
		action, _ := decodeWire(run.data(2))
		userAction, _ := decodeWire(action.data(1))
		user, _ := decodeWire(userAction.data(1))
		mu.Lock()
		prompts = append(prompts, string(user.data(1)))
		turn := len(prompts)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/connect+proto")
		if turn == 1 {
			// The upstream requests its MCP catalog and expects a correlated reply.
			_ = writeFrame(w, bytesField(2, fields(numberField(1, 9), textField(15, "catalog"), bytesField(36, nil))))
			w.(http.Flusher).Flush()
			_, reply, err := readFrame(req.Body)
			if err != nil || !bytes.Contains(reply, []byte(clientToolProvider)) || !bytes.Contains(reply, []byte("read_file")) {
				t.Error("missing client tool catalog reply")
				return
			}
			value, _ := structpb.NewValue("README.md")
			encoded, _ := proto.Marshal(value)
			args := fields(textField(4, clientToolProvider), textField(5, "read_file"), bytesField(2, fields(textField(1, "path"), bytesField(2, encoded))))
			_ = writeFrame(w, bytesField(2, fields(numberField(1, 10), bytesField(11, args))))
			w.(http.Flusher).Flush()
			<-req.Context().Done()
			return
		}
		_ = writeFrame(w, textPacket("The file says hello."))
		_ = writeFrame(w, donePacket())
		w.(http.Flusher).Flush()
	})
	chat := basicChat()
	chat["tools"] = []tool{{Type: "function", Function: function{Name: "read_file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)}}}
	_, c, credential, err := parseExecution(envelope(t, chat))
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.execute(context.Background(), execution{}, c, credential)
	if err != nil {
		t.Fatal(err)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				ToolCalls []toolCall `json:"tool_calls"`
			}
			Finish string `json:"finish_reason"`
		}
	}
	if err := json.Unmarshal(result.(map[string]any)["Payload"].([]byte), &completion); err != nil {
		t.Fatal(err)
	}
	if len(completion.Choices) != 1 || len(completion.Choices[0].Message.ToolCalls) != 1 || completion.Choices[0].Finish != "tool_calls" {
		t.Fatalf("bad handoff: %+v", completion)
	}
	call := completion.Choices[0].Message.ToolCalls[0]
	if call.Function.Arguments != `{"path":"README.md"}` || len(call.ID) > 64 {
		t.Fatalf("bad tool call: %+v", call)
	}
	chat["messages"] = append(chat["messages"].([]any), map[string]any{"role": "assistant", "tool_calls": []toolCall{call}}, map[string]any{"role": "tool", "tool_call_id": call.ID, "content": "hello"})
	_, c, credential, err = parseExecution(envelope(t, chat))
	if err != nil {
		t.Fatal(err)
	}
	result, err = s.execute(context.Background(), execution{}, c, credential)
	if err != nil || !bytes.Contains(result.(map[string]any)["Payload"].([]byte), []byte("The file says hello.")) {
		t.Fatalf("continuation failed: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(prompts) != 2 || !strings.Contains(prompts[1], call.ID) || !strings.Contains(prompts[1], `"role":"tool"`) {
		t.Fatal("completed tool history was not included")
	}
}

func TestStreamingAndLimits(t *testing.T) {
	for _, stream := range []string{"complete", "budget", "broken", "empty", "callback failure"} {
		t.Run(stream, func(t *testing.T) {
			s := testService(t, func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/connect+proto")
				w.Header().Set("X-Content-Type-Options", "nosniff")
				_, _, _ = readFrame(req.Body)
				if stream != "empty" {
					_ = writeFrame(w, textPacket("hello world"))
				}
				if stream == "broken" {
					_, _ = io.Copy(w, bytes.NewReader([]byte{0, 0, 0}))
				} else {
					_ = writeFrame(w, donePacket())
				}
				w.(http.Flusher).Flush()
			})
			var frames [][]byte
			var closeBody []byte
			s.host = func(method string, v any) (json.RawMessage, error) {
				body := v.(map[string]any)
				if method == "host.stream.emit" {
					if stream == "callback failure" {
						return nil, errors.New("client disconnected")
					}
					frames = append(frames, body["payload"].([]byte))
				} else {
					closeBody = marshal(t, body)
				}
				return json.RawMessage(`{}`), nil
			}
			chat := basicChat()
			if stream == "budget" {
				chat["max_tokens"] = 5
			}
			_, c, credential, err := parseExecution(envelope(t, chat))
			if err != nil {
				t.Fatal(err)
			}
			s.executeStream(context.Background(), execution{StreamID: "stream-1"}, c, credential)
			if stream == "broken" || stream == "empty" || stream == "callback failure" {
				if !bytes.Contains(closeBody, []byte(`"error_details"`)) {
					t.Fatalf("failure lost at stream boundary: %s", closeBody)
				}
				for _, frame := range frames {
					if string(frame) == "[DONE]" {
						t.Fatal("failed stream reported successful completion")
					}
				}
				return
			}
			if len(frames) < 2 || string(frames[len(frames)-1]) != "[DONE]" {
				t.Fatal("missing final SSE marker")
			}
			if stream == "budget" && (!bytes.Contains(frames[0], []byte(`"content":"hello"`)) || !bytes.Contains(frames[len(frames)-2], []byte(`"finish_reason":"length"`))) {
				t.Fatal("output limit did not truncate and finish with length")
			}
		})
	}
}

func TestProtocolBoundsAndRefusals(t *testing.T) {
	var header [5]byte
	binary.BigEndian.PutUint32(header[1:], maxFrameBytes+1)
	if _, _, err := readFrame(bytes.NewReader(header[:])); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if _, _, err := readFrame(bytes.NewReader([]byte{0, 0})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("truncated frame accepted")
	}
	p := protocolState{}
	if reply, err := p.receive(bytesField(2, fields(numberField(1, 1), bytesField(2, textField(1, "touch /tmp/never")))), chatRequest{}); err != nil || len(reply.Replies) != 1 || !bytes.Contains(reply.Replies[0], []byte(nativePolicy)) {
		t.Fatal("native shell was not explicitly refused")
	}
	if _, err := p.receive([]byte{0xff}, chatRequest{}); err == nil {
		t.Fatal("malformed protobuf accepted")
	}
	for _, number := range []int{2, 3, 4, 5, 6, 7} {
		reply, err := interactionRefusal(fields(numberField(1, 7), bytesField(number, nil)))
		if err != nil || !bytes.Contains(reply, []byte("cannot approve")) {
			t.Fatalf("interaction %d was not refused", number)
		}
	}
	key := bytes.Repeat([]byte{'k'}, 4<<20)
	if _, err := p.blob(fields(numberField(1, 1), bytesField(3, fields(bytesField(1, key), bytesField(2, []byte{'x'}))))); err == nil || len(p.blobs) > 0 {
		t.Fatal("blob limit counted only data or changed state on rejected write")
	}
	if trailerError([]byte(`{"error":{"code":"resource_exhausted","message":"SECRET"}}`)).Error() == "SECRET" {
		t.Fatal("upstream response body leaked")
	}
	a := answer{limit: 2, reason: "stop"}
	text, err := a.accept(event{Text: "€"})
	if err != nil || text != "" || a.reason != "length" {
		t.Fatal("UTF-8 output was split")
	}
}

func TestCancellationAndAdmission(t *testing.T) {
	stopped := make(chan struct{})
	s := testService(t, func(w http.ResponseWriter, req *http.Request) {
		_, _, _ = readFrame(req.Body)
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-req.Context().Done()
		close(stopped)
	})
	_, c, credential, err := parseExecution(envelope(t, basicChat()))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.run(ctx, c, credential.AccessToken, func(event) (bool, error) { return false, nil }); err == nil {
		t.Fatal("stalled transport did not cancel")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("cancelled run left its HTTP/2 stream alive")
	}
	var releases []func()
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for i := 0; i < cap(s.slots); i++ {
		_, release, err := s.begin()
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	if _, _, err := s.begin(); err == nil {
		t.Fatal("operation admission limit was exceeded")
	}
	for _, release := range releases {
		release()
	}
	releases = nil
	s.Shutdown()
	if _, _, err := s.begin(); err == nil {
		t.Fatal("request accepted after shutdown")
	}
}

func TestOAuthAndModelDiscovery(t *testing.T) {
	claims := marshal(t, map[string]any{"sub": "account-42", "email": "local@example.test", "exp": time.Now().Add(time.Hour).Unix()})
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	var polls int
	s := testService(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		switch req.URL.Path {
		case "/auth/poll":
			polls++
			if req.URL.Query().Get("uuid") == "" || req.URL.Query().Get("verifier") == "" {
				t.Error("missing PKCE poll correlation")
			}
			if polls == 1 {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": token, "refreshToken": "refresh"})
		case "/auth/exchange_user_api_key":
			if req.Header.Get("Authorization") != "Bearer refresh" {
				t.Error("wrong refresh credential")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": token})
		case "/agent.v1.AgentService/GetUsableModels":
			w.Header().Set("Content-Type", "application/proto")
			_, _ = io.Copy(w, bytes.NewReader(fields(bytesField(1, textField(1, "test-model")), bytesField(1, textField(1, "test-model")))))
		default:
			w.WriteHeader(404)
		}
	})
	start, err := s.startLogin()
	if err != nil {
		t.Fatal(err)
	}
	state := start.(map[string]any)["State"].(string)
	if !strings.HasPrefix(start.(map[string]any)["URL"].(string), "https://cursor.com/loginDeepControl?") {
		t.Fatal("login does not use Cursor's website")
	}
	pollRaw := marshal(t, map[string]string{"State": state})
	pending, err := s.pollLogin(context.Background(), pollRaw)
	if err != nil || pending.(map[string]string)["Status"] != "pending" {
		t.Fatal("pending login failed")
	}
	result, err := s.pollLogin(context.Background(), pollRaw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pollLogin(context.Background(), pollRaw); err == nil {
		t.Fatal("successful state reused")
	}
	auth := result.(map[string]any)["Auth"].(map[string]any)
	if auth["Provider"] != ID || !strings.HasSuffix(auth["FileName"].(string), ".json") {
		t.Fatal("invalid native auth registration")
	}
	raw := marshal(t, map[string]any{"StorageJSON": auth["StorageJSON"]})
	refreshed, err := s.refresh(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	nextAuth := refreshed.(map[string]any)["Auth"].(map[string]any)
	if _, exists := nextAuth["ID"]; exists {
		t.Fatal("refresh attempted to create a new account")
	}
	next, err := parseCredentials(nextAuth["StorageJSON"].([]byte))
	if err != nil || next.RefreshToken != "refresh" {
		t.Fatal("refresh token fallback failed")
	}
	models, err := s.models(context.Background(), raw)
	if err != nil || len(models.(map[string]any)["Models"].([]any)) != 1 {
		t.Fatal("model discovery failed or did not deduplicate")
	}
}
