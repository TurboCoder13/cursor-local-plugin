package provider

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const cursorClientVersion = "cli-2026.09.02-c22c1a3"

func headers(req *http.Request, token, contentType string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("TE", "trailers")
	req.Header.Set("X-Cursor-Client-Type", "cli")
	req.Header.Set("X-Cursor-Client-Version", cursorClientVersion)
	req.Header.Set("X-Ghost-Mode", "true")
	req.Header.Set("X-Session-Id", randomID())
	req.Header.Set("X-Request-Id", randomID())
}

func (s *Service) models(ctx context.Context, raw []byte) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var request struct{ StorageJSON []byte }
	if json.Unmarshal(raw, &request) != nil {
		return nil, reject(400, "invalid model discovery request")
	}
	c, err := parseCredentials(request.StorageJSON)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/agent.v1.AgentService/GetUsableModels", http.NoBody)
	if err != nil {
		return nil, reject(500, "cannot construct model discovery")
	}
	headers(req, c.AccessToken, "application/proto")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, reject(502, "Cursor model discovery could not be reached")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, statusError(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(body) > 4<<20 {
		return nil, reject(502, "Cursor model response exceeds the limit or is incomplete")
	}
	w, err := decodeWire(body)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool)
	for _, item := range w[1] {
		model, err := decodeWire(item.data)
		if err != nil || item.kind != 2 {
			return nil, malformedProtocol()
		}
		id := string(model.data(1))
		if id != "" && len(id) <= 128 {
			ids[id] = true
		}
	}
	if len(ids) == 0 {
		return nil, reject(502, "Cursor returned no usable models")
	}
	capacity := s.discoverCapacity(ctx, c.AccessToken)
	s.mu.Lock()
	s.capacities[c.AccountID] = capacity
	s.mu.Unlock()
	for _, disabled := range c.DisabledModels {
		delete(ids, strings.TrimPrefix(disabled, "cursor/"))
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	models := make([]any, 0, len(ids))
	for _, id := range ordered {
		model := map[string]any{"ID": "cursor/" + id, "Object": "model", "OwnedBy": ID, "DisplayName": "Cursor · " + id,
			"SupportedInputModalities": []string{"text", "image"}, "SupportedOutputModalities": []string{"text", "image"}, "SupportedGenerationMethods": []string{"chat-completions"}, "ClientContextLimit": 1000000}
		if capacity[id] > 0 {
			model["NativeContextLength"] = capacity[id]
			model["ContextLength"] = min(capacity[id], 1000000)
		}
		models = append(models, model)
	}
	return map[string]any{"Provider": ID, "Models": models}, nil
}

func writeFrame(writer io.Writer, payload []byte) error {
	header := make([]byte, 5)
	binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
	_, err := io.Copy(writer, bytes.NewReader(append(header, payload...)))
	return err
}

func readFrame(reader io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return 0, nil, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > maxFrameBytes {
		return 0, nil, reject(502, "Cursor frame exceeds 24 MiB")
	}
	body := make([]byte, int(length))
	_, err := io.ReadFull(reader, body)
	return header[0], body, err
}

func trailerError(raw []byte) error {
	var trailer struct {
		Error *struct{ Code string }
	}
	if json.Unmarshal(raw, &trailer) != nil {
		return malformedProtocol()
	}
	if trailer.Error == nil {
		return nil
	}
	switch trailer.Error.Code {
	case "resource_exhausted":
		return statusError(429)
	case "unauthenticated":
		return statusError(401)
	case "permission_denied":
		return statusError(403)
	case "invalid_argument", "internal", "failed_precondition":
		f := reject(502, "Cursor upstream rejected the checkpoint or run state").(*failure)
		f.Replayable = true
		return f
	default:
		return reject(502, "Cursor upstream ended the run with a protocol error")
	}
}

func (s *Service) run(parent context.Context, c chatRequest, token string, consume func(event) (bool, error)) error {
	packet, err := runPacket(c)
	if err != nil {
		return err
	}
	ctx, cancelCause := context.WithCancelCause(parent)
	cancel := func() { cancelCause(context.Canceled) }
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	replies := make(chan []byte, 8)
	written := make(chan error, 1)
	go func() {
		err := writeFrame(writer, packet)
		if err == nil {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for err == nil {
				select {
				case <-ctx.Done():
					err = ctx.Err()
				case <-ticker.C:
					err = writeFrame(writer, bytesField(7, nil))
				case reply := <-replies:
					err = writeFrame(writer, reply)
				}
			}
		}
		_ = writer.CloseWithError(err)
		written <- err
	}()
	defer func() {
		cancel()
		_ = reader.Close()
		_ = writer.Close()
		<-written
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/agent.v1.AgentService/Run", reader)
	if err != nil {
		return reject(500, "cannot construct Cursor run")
	}
	headers(req, token, "application/connect+proto")
	resp, err := s.client.Do(req)
	if err != nil {
		return reject(502, "Cursor run could not be reached")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode == 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			var detail struct{ Code string }
			if json.Unmarshal(raw, &detail) == nil && detail.Code == "invalid_argument" {
				f := reject(502, "Cursor rejected run arguments").(*failure)
				f.Replayable = true
				return f
			}
		}
		return statusError(resp.StatusCode)
	}
	silence := time.AfterFunc(30*time.Second, func() { cancelCause(reject(504, "Cursor frame silence exceeded 30 seconds")) })
	progress := time.AfterFunc(90*time.Second, func() {
		f := reject(504, "Cursor meaningful progress exceeded 90 seconds").(*failure)
		f.Replayable = true
		cancelCause(f)
	})
	defer silence.Stop()
	defer progress.Stop()
	state := protocolState{}
	type received struct {
		flag  byte
		frame []byte
		err   error
	}
	incoming := make(chan received, 1)
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		for {
			flag, frame, err := readFrame(resp.Body)
			select {
			case incoming <- received{flag, frame, err}:
			case <-stopped:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var drain *time.Timer
	defer func() {
		if drain != nil {
			drain.Stop()
		}
	}()
	var drainC <-chan time.Time
	terminal, hadOutput := false, false
	for {
		var next received
		select {
		case <-drainC:
			if hadOutput {
				return nil
			}
			return emptyResponse()
		case <-ctx.Done():
			if cause, ok := context.Cause(ctx).(*failure); ok {
				return cause
			}
			return reject(504, "Cursor run timed out or was stopped")
		case next = <-incoming:
		}
		flag, frame, err := next.flag, next.frame, next.err
		if err != nil {
			if terminal && hadOutput && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
				return nil
			}
			var f *failure
			if errors.As(err, &f) {
				return f
			}
			return reject(502, "Cursor stream ended before a complete response")
		}
		silence.Reset(30 * time.Second)
		if flag == 2 {
			if err := trailerError(frame); err != nil {
				return err
			}
			if !hadOutput {
				return emptyResponse()
			}
			return nil
		}
		if flag != 0 {
			return reject(502, "unsupported Cursor frame flags")
		}
		ev, err := state.receive(frame, c)
		if err != nil {
			return err
		}
		if terminal && ev.Call == nil && len(ev.Checkpoint) == 0 {
			continue
		}
		for _, reply := range ev.Replies {
			select {
			case replies <- reply:
			case <-ctx.Done():
				return reject(504, "Cursor reply timed out")
			}
		}
		if ev.Text != "" || ev.Call != nil || ev.Image != "" {
			hadOutput = true
		}
		if ev.Text != "" || ev.Call != nil || ev.Image != "" || ev.Thinking || ev.Done {
			progress.Reset(90 * time.Second)
		}
		finished, err := consume(ev)
		if err != nil || finished {
			return err
		}
		// Collect sibling MCP calls and trailing checkpoints before closing the stream.
		if (ev.Done || ev.Call != nil) && !terminal {
			terminal = true
			drain = time.NewTimer(100 * time.Millisecond)
			drainC = drain.C
		}
	}
}

func emptyResponse() error {
	f := reject(502, "Cursor returned an empty response").(*failure)
	f.Replayable = true
	return f
}
