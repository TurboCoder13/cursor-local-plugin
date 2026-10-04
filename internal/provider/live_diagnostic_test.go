package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type diagnosticTransport struct {
	Underlying http.RoundTripper
	Record     func(string, map[string]bool)
}

func (d diagnosticTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := d.Underlying.RoundTrip(r)
	if err == nil {
		response.Body = &diagnosticBody{ReadCloser: response.Body, Record: d.Record}
	}
	return response, err
}

type diagnosticBody struct {
	io.ReadCloser
	Buffer   []byte
	Record   func(string, map[string]bool)
	Recorded bool
}

func (d *diagnosticBody) Read(p []byte) (int, error) {
	n, err := d.ReadCloser.Read(p)
	if len(d.Buffer)+n <= maxFrameBytes {
		d.Buffer = append(d.Buffer, p[:n]...)
	}
	data := d.Buffer
	for len(data) >= 5 {
		size := int(data[1])<<24 | int(data[2])<<16 | int(data[3])<<8 | int(data[4])
		if size > len(data)-5 {
			break
		}
		if data[0] == 2 && !d.Recorded {
			var trailer struct {
				Error *struct{ Code, Message string }
			}
			if json.Unmarshal(data[5:5+size], &trailer) == nil && trailer.Error != nil {
				flags := make(map[string]bool)
				for _, word := range []string{"model", "quota", "billing", "account", "token", "context", "proto", "request", "version", "invalid", "unsupported", "unauthorized", "workspace", "run_id", "timeout", "rate", "user", "connection", "deadline", "excludeworkspacecontext", "exclude_workspace_context", "must", "cannot", "required", "missing", "deprecated", "permission", "not allowed", "not supported", "requestcontext", "request_context", "include", "exclude", "non-empty", "empty"} {
					flags[word] = strings.Contains(strings.ToLower(trailer.Error.Message), word)
				}
				d.Record(trailer.Error.Code, flags)
				d.Recorded = true
			}
		}
		data = data[5+size:]
	}
	return n, err
}
func TestLiveDiagnostic(t *testing.T) {
	model := os.Getenv("CURSOR_LOCAL_LIVE_MODEL")
	if model == "" {
		t.Skip("explicit live diagnostic model required")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("home unavailable")
	}
	paths, _ := filepath.Glob(filepath.Join(home, ".cli-proxy-api", "*.json"))
	var credential credentials
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err == nil {
			if parsed, err := parseCredentials(raw); err == nil {
				credential = parsed
				break
			}
		}
	}
	if credential.AccessToken == "" {
		t.Fatal("no persisted Cursor account")
	}
	s := NewService(nil)
	defer s.Shutdown()
	s.client.Transport = diagnosticTransport{Underlying: s.client.Transport, Record: func(code string, flags map[string]bool) {
		t.Logf("upstream code=%s classified keywords=%v", code, flags)
	}}
	c := chatRequest{Model: model, Prompt: gatewayPrompt + `{"role":"user","content":"Reply PROXY_OK"}`, OutputLimit: 1024, ConversationID: randomID()}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var received strings.Builder
	err = s.run(ctx, c, credential.AccessToken, func(ev event) (bool, error) {
		if ev.Text != "" {
			received.WriteString(ev.Text)
		}
		return false, nil
	})
	if err == nil && !strings.Contains(received.String(), "PROXY_OK") {
		t.Fatal("expected marker absent from completion")
	}
	if err != nil {
		if f, ok := err.(*failure); ok {
			t.Fatalf("sanitized provider failure: %s", f.Message)
		}
		t.Fatal("unclassified failure")
	}
}
