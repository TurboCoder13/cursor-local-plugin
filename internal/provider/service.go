// Package provider implements an independently written Cursor provider.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	ID               = "cursor-local"
	Version          = "0.2.4"
	MaxEnvelopeBytes = 8 << 20
	maxPayloadBytes  = 1000000 - 16384
	maxFrameBytes    = 24 << 20
	maxOutputBytes   = 16 << 20
	maxRunTime       = 15 * time.Minute
)

type HostCall func(string, any) (json.RawMessage, error)

type Service struct {
	host        HostCall
	client      *http.Client
	base        string
	root        context.Context
	stop        context.CancelFunc
	slots       chan struct{}
	mu          sync.Mutex
	closed      bool
	active      sync.WaitGroup
	logins      map[string]login
	checkpoints map[string]checkpoint
	turns       map[string]*turnQueue
	waiting     int
	cacheStats  cacheCounters
	cacheUsage  map[string]*cacheCounters
	usage       map[string]*usageRecord
	completions completionFilter
	selections  map[string]selection
	capacities  map[string]map[string]int64
	quotas      map[[32]byte]*quotaSnapshot
}

type failure struct {
	Code                 string `json:"code"`
	Message              string `json:"message"`
	HTTPStatus           int    `json:"http_status"`
	RequestScoped        bool   `json:"request_scoped,omitempty"`
	OutputExposed        bool   `json:"output_exposed,omitempty"`
	ToolExposed          bool   `json:"tool_exposed,omitempty"`
	InteractionResponded bool   `json:"interaction_responded,omitempty"`
	Replayable           bool   `json:"-"`
}

func (f *failure) Error() string { return f.Message }

func reject(status int, message string) error {
	return &failure{Code: "cursor_local_error", Message: message, HTTPStatus: status, RequestScoped: status == 400 || status == 409 || status == 413}
}

func NewService(host HostCall) *Service {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ForceAttemptHTTP2 = true
	transport.ResponseHeaderTimeout = 30 * time.Second
	root, stop := context.WithCancel(context.Background())
	return &Service{
		host: host, base: "https://api2.cursor.sh", root: root, stop: stop,
		client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Cursor redirect refused")
		}},
		slots: make(chan struct{}, 128), logins: make(map[string]login), checkpoints: make(map[string]checkpoint), turns: make(map[string]*turnQueue), usage: make(map[string]*usageRecord), cacheUsage: make(map[string]*cacheCounters), selections: make(map[string]selection), capacities: make(map[string]map[string]int64),
	}
}

func (s *Service) Shutdown() {
	s.mu.Lock()
	s.closed = true
	s.stop()
	s.mu.Unlock()
	s.active.Wait()
	s.client.CloseIdleConnections()
}

func (s *Service) begin() (context.Context, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, reject(503, "Cursor provider is stopping")
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return nil, nil, reject(409, "Cursor provider has reached its operation limit")
	}
	s.active.Add(1)
	ctx, cancel := context.WithTimeout(s.root, maxRunTime)
	return ctx, func() { cancel(); <-s.slots; s.active.Done() }, nil
}

func encode(result any, err error) ([]byte, bool) {
	if err != nil {
		var f *failure
		if !errors.As(err, &f) {
			f = &failure{Code: "cursor_local_error", Message: "Cursor operation failed", HTTPStatus: 502}
		}
		raw, _ := json.Marshal(map[string]any{"ok": false, "error": f})
		return raw, false
	}
	raw, marshalErr := json.Marshal(map[string]any{"ok": true, "result": result})
	if marshalErr != nil {
		return encode(nil, reject(500, "cannot encode Cursor result"))
	}
	return raw, true
}

func (s *Service) Call(method string, raw []byte) ([]byte, bool) {
	if len(raw) > MaxEnvelopeBytes {
		return encode(nil, reject(413, "plugin envelope exceeds 8 MiB"))
	}
	if strings.HasPrefix(method, "executor.") && len(raw) > 4<<20 {
		return encode(nil, reject(400, "executor envelope exceeds 4 MiB"))
	}
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return encode(map[string]any{
			"schema_version": 6,
			"metadata":       map[string]any{"Name": ID, "Version": Version, "Author": "Local", "GitHubRepository": "local://cursor-local-plugin", "ConfigFields": []string{}},
			"capabilities": map[string]any{
				"auth_provider": true, "model_provider": true, "executor": true, "quota_provider": true,
				"executor_model_scope": "oauth", "executor_input_formats": []string{"chat-completions"},
				"executor_output_formats": []string{"chat-completions"}, "management_api": true, "request_lifecycle_plugin": true, "request_interceptor": true,
			},
		}, nil)
	case "management.register":
		return encode(managementRegistration(), nil)
	case "management.handle":
		ctx, finish, err := s.begin()
		if err != nil {
			return encode(nil, err)
		}
		defer finish()
		return encode(s.managementWithContext(ctx, raw))
	case "request.intercept_before", "request.intercept_after", "request.complete":
		return encode(s.lifecycle(method, raw))
	case "auth.identifier", "executor.identifier", "quota.identifier":
		return encode(map[string]string{"identifier": ID}, nil)
	case "quota.describe":
		return encode(map[string]any{"supported_providers": []string{ID}, "display_name": "Cursor Local", "supports_reset": false}, nil)
	case "model.static":
		return encode(map[string]any{"Provider": ID, "Models": []any{}}, nil)
	case "auth.parse":
		return encode(s.parseAuth(raw))
	case "plugin.quiesce", "plugin.shutdown":
		s.Shutdown()
		return encode(struct{}{}, nil)
	}
	ctx, finish, err := s.begin()
	if err != nil {
		return encode(nil, err)
	}
	if method == "executor.execute_stream" {
		request, chat, credential, err := parseExecution(raw)
		if err != nil || request.StreamID == "" || s.host == nil {
			finish()
			if err == nil {
				err = reject(400, "stream callback is required")
			}
			return encode(nil, err)
		}
		go func() { defer finish(); s.executeStream(ctx, request, chat, credential) }()
		return encode(map[string]any{"headers": http.Header{"Content-Type": {"text/event-stream"}}}, nil)
	}
	defer finish()
	var result any
	switch method {
	case "auth.login.start":
		result, err = s.startLogin()
	case "auth.login.poll":
		result, err = s.pollLogin(ctx, raw)
	case "auth.refresh":
		result, err = s.refresh(ctx, raw)
	case "model.for_auth":
		result, err = s.models(ctx, raw)
	case "executor.execute":
		var request execution
		var chat chatRequest
		var credential credentials
		request, chat, credential, err = parseExecution(raw)
		if err == nil {
			result, err = s.execute(ctx, request, chat, credential)
		}
	case "quota.fetch":
		result, err = s.fetchQuota(ctx, raw)
	case "quota.reset":
		err = reject(400, "Cursor subscription quota cannot be reset by this plugin")
	case "executor.count_tokens":
		result, err = s.countTokens(raw)
	default:
		err = reject(400, "unsupported Cursor plugin method")
	}
	return encode(result, err)
}
