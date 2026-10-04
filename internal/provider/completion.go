package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type answer struct {
	text       strings.Builder
	calls      []toolCall
	reason     string
	limit      int
	used       int
	images     []any
	input      int
	imageBytes int
}

func (a *answer) accept(event event) (string, error) {
	if event.ThinkingText != "" {
		a.used += min(len(event.ThinkingText), max(0, a.limit-a.used))
		if a.used >= a.limit {
			a.reason = "length"
		}
		return "", nil
	}
	if event.Image != "" {
		if a.imageBytes+len(event.Image) > 64<<20 {
			return "", reject(502, "image output budget exceeded")
		}
		a.imageBytes += len(event.Image)
		a.images = append(a.images, map[string]any{"type": "image_url", "image_url": map[string]string{"url": event.Image}})
		return "", nil
	}
	if event.Call != nil {
		size := len(event.Call.Function.Arguments) + len(event.Call.Function.Name) + len(event.Call.ID)
		if size > a.limit-a.used {
			a.reason = "length"
			return "", nil
		}
		a.used += size
		a.calls = append(a.calls, *event.Call)
		a.reason = "tool_calls"
		return "", nil
	}
	text := event.Text
	remaining := max(0, a.limit-a.used)
	if len(text) >= remaining && len(text) > 0 {
		// Keep only whole UTF-8 codepoints at the budget boundary.
		cut := remaining
		for cut > 0 && cut < len(text) && (text[cut]&0xc0) == 0x80 {
			cut--
		}
		text = text[:cut]
		a.reason = "length"
	}
	a.text.WriteString(text)
	a.used += len(text)
	return text, nil
}

func (a *answer) response(model, id string, created int64, delta any, final bool) map[string]any {
	object := "chat.completion"
	choice := map[string]any{"index": 0, "finish_reason": a.reason}
	if delta != nil || !final {
		object = "chat.completion.chunk"
		choice["delta"] = delta
		if !final {
			choice["finish_reason"] = nil
		}
	} else {
		m := map[string]any{"role": "assistant", "content": a.text.String()}
		if len(a.calls) > 0 {
			m["tool_calls"] = a.calls
		}
		if len(a.images) > 0 {
			m["images"] = a.images
		}
		choice["message"] = m
	}
	response := map[string]any{"id": id, "object": object, "created": created, "model": "cursor/" + model, "choices": []any{choice}}
	if final {
		response["usage"] = map[string]any{"prompt_tokens": a.input, "completion_tokens": (a.used + 3) / 4, "total_tokens": a.input + (a.used+3)/4, "estimated": true}
	}
	return response
}

func (s *Service) execute(ctx context.Context, request execution, c chatRequest, credential credentials) (any, error) {
	a := answer{reason: "stop", limit: c.OutputLimit, input: estimatedInput(c)}
	defer func() { s.recordExecution(request.AuthID, a.input, (a.used+3)/4) }()
	err := s.runCheckpointed(ctx, request, c, credential, func(e event) (bool, error) {
		_, err := a.accept(e)
		return a.reason == "length", err
	})
	if err != nil {
		return nil, err
	}
	if a.text.Len() == 0 && len(a.calls) == 0 && len(a.images) == 0 && a.reason != "length" {
		return nil, reject(502, "Cursor returned an empty completion")
	}
	body, err := json.Marshal(a.response(c.Model, "chatcmpl-"+randomID(), time.Now().Unix(), nil, true))
	return map[string]any{"Payload": body, "Headers": http.Header{"Content-Type": {"application/json"}}}, err
}

func (s *Service) executeStream(ctx context.Context, request execution, c chatRequest, credential credentials) {
	a := answer{reason: "stop", limit: c.OutputLimit, input: estimatedInput(c)}
	defer func() { s.recordExecution(request.AuthID, a.input, (a.used+3)/4) }()
	id, created := "chatcmpl-"+randomID(), time.Now().Unix()
	exposed := false
	emit := func(delta any, final bool) error {
		body, err := json.Marshal(a.response(c.Model, id, created, delta, final))
		if err == nil {
			_, err = s.host("host.stream.emit", map[string]any{"stream_id": request.StreamID, "payload": body})
		}
		if err == nil {
			exposed = true
		}
		return err
	}
	err := s.runCheckpointed(ctx, request, c, credential, func(e event) (bool, error) {
		text, err := a.accept(e)
		if err != nil {
			return false, err
		}
		if e.Image != "" {
			err = emit(map[string]any{"role": "assistant", "images": []any{a.images[len(a.images)-1]}}, false)
		} else if e.Call != nil && a.reason != "length" {
			call := map[string]any{"index": len(a.calls) - 1, "id": e.Call.ID, "type": "function", "function": e.Call.Function}
			err = emit(map[string]any{"role": "assistant", "tool_calls": []any{call}}, false)
		} else if text != "" {
			err = emit(map[string]any{"role": "assistant", "content": text}, false)
		}
		return a.reason == "length", err
	})
	if err == nil && a.text.Len() == 0 && len(a.calls) == 0 && len(a.images) == 0 && a.reason != "length" {
		err = reject(502, "Cursor returned an empty completion")
	}
	if err == nil {
		err = emit(map[string]any{}, true)
	}
	if err == nil {
		_, err = s.host("host.stream.emit", map[string]any{"stream_id": request.StreamID, "payload": []byte("[DONE]")})
	}
	closeRequest := map[string]any{"stream_id": request.StreamID}
	if err != nil {
		var f *failure
		if !errors.As(err, &f) {
			f = &failure{Code: "cursor_local_error", Message: "Cursor stream failed", HTTPStatus: 502}
		}
		closeRequest["error"] = f.Message
		closeRequest["error_details"] = map[string]any{"code": f.Code, "message": f.Message, "http_status": f.HTTPStatus, "output_exposed": exposed || f.OutputExposed, "tool_exposed": f.ToolExposed, "interaction_responded": f.InteractionResponded, "request_scoped": f.RequestScoped}
	}
	_, _ = s.host("host.stream.close", closeRequest)
}

func supportedTool(c chatRequest, name string) bool {
	for _, t := range c.Tools {
		if t.Function.Name == name {
			return true
		}
	}
	return false
}

func malformedProtocol() error {
	return reject(502, "Cursor returned an invalid or unsupported protocol message")
}

func unsupportedOperation(number int) error {
	return reject(502, fmt.Sprintf("Cursor requested unsupported native operation %d; use client function tools", number))
}

func estimatedInput(c chatRequest) int {
	size := len(c.Prompt)
	for _, a := range c.Attachments {
		size += len(a.Text)
	}
	for _, t := range c.Tools {
		size += len(t.Function.Name) + len(t.Function.Description) + len(t.Function.Parameters)
	}
	return (size + 3) / 4
}
