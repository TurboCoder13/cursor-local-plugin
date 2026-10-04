package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

const nativePolicy = "Native gateway access is unavailable. Use a declared client MCP tool or return the request to the client; do not repeat the native operation."

func execReply(w wire, number int, result []byte) []byte {
	return bytesField(2, fields(numberField(1, w.num(1)), bytesField(15, w.data(15)), bytesField(number, result)))
}
func shellRefusal(w wire, operation int) (event, error) {
	args, err := decodeWire(w.data(operation))
	if err != nil {
		return event{}, err
	}
	failure := bytesField(2, fields(bytesField(1, args.data(1)), bytesField(2, args.data(2)), numberField(3, 1), textField(6, nativePolicy), numberField(11, 1)))
	reply := execReply(w, 2, failure)
	if operation == 2 {
		return event{Replies: [][]byte{reply}}, nil
	}
	return event{Replies: [][]byte{
		execReply(w, 14, bytesField(4, nil)),
		execReply(w, 14, bytesField(2, textField(1, nativePolicy))),
		execReply(w, 14, bytesField(3, fields(numberField(1, 1), bytesField(2, args.data(2)), numberField(4, 1)))),
		reply, bytesField(5, bytesField(1, numberField(1, w.num(1)))),
	}}, nil
}

func (p *protocolState) mcp(args wire, c chatRequest, override string) (event, error) {
	name := string(args.data(5))
	if name == "" {
		name = string(args.data(1))
	}
	if string(args.data(4)) != clientToolProvider || !supportedTool(c, name) {
		return event{}, reject(502, "Cursor requested an undeclared client tool")
	}
	params := make(map[string]any)
	for _, entry := range args[2] {
		item, err := decodeWire(entry.data)
		if err != nil {
			return event{}, err
		}
		raw := item.data(2)
		var value structpb.Value
		var decoded any
		if proto.Unmarshal(raw, &value) == nil && value.Kind != nil && len(value.ProtoReflect().GetUnknown()) == 0 {
			decoded = value.AsInterface()
			if text, ok := decoded.(string); ok {
				var nested any
				if json.Unmarshal([]byte(text), &nested) == nil {
					decoded = nested
				}
			}
		} else if json.Unmarshal(raw, &decoded) != nil {
			decoded = string(raw)
		}
		params[string(item.data(1))] = decoded
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return event{}, malformedProtocol()
	}
	rawID := string(args.data(3))
	if override != "" {
		rawID = override
	}
	if rawID == "" {
		rawID = "exec_" + randomID()
	}
	if p.ids == nil {
		p.ids = make(map[string]string)
		p.emitted = make(map[string]bool)
	}
	id, exists := p.ids[rawID]
	if !exists {
		id = stableToolID(rawID)
		for previous, candidate := range p.ids {
			if candidate == id && previous != rawID {
				id = stableToolID(rawID + "\x00" + randomID())
			}
		}
		p.ids[rawID] = id
	}
	if p.emitted[id] {
		return event{}, nil
	}
	p.emitted[id] = true
	return event{Call: &toolCall{ID: id, Type: "function", Function: function{Name: name, Arguments: string(encoded)}}}, nil
}
func (p *protocolState) completed(raw []byte, c chatRequest) (event, error) {
	completed, err := decodeWire(raw)
	if err != nil {
		return event{}, err
	}
	call, err := decodeWire(completed.data(2))
	if err != nil {
		return event{}, err
	}
	if _, exists := call[15]; exists {
		mcp, err := decodeWire(call.data(15))
		if err != nil {
			return event{}, err
		}
		args, err := decodeWire(mcp.data(1))
		if err != nil {
			return event{}, err
		}
		return p.mcp(args, c, string(completed.data(1)))
	}
	if _, exists := call[28]; !exists {
		return event{}, nil
	}
	generated, err := decodeWire(call.data(28))
	if err != nil {
		return event{}, err
	}
	result, err := decodeWire(generated.data(2))
	if err != nil {
		return event{}, err
	}
	if _, exists := result[1]; !exists {
		return event{}, reject(502, "Cursor image generation failed")
	}
	success, err := decodeWire(result.data(1))
	if err != nil {
		return event{}, err
	}
	encoded := string(success.data(2))
	if len(encoded) > 24<<20 || encoded == "" {
		return event{}, reject(502, "invalid or oversized generated image")
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	media := http.DetectContentType(data)
	if err != nil || len(data) > 16<<20 || !strings.HasPrefix(media, "image/") {
		return event{}, reject(502, "generated data is not a supported image")
	}
	return event{Image: "data:" + media + ";base64," + base64.StdEncoding.EncodeToString(data)}, nil
}

// A bounded virtual image workspace satisfies the protocol without host writes.
func (p *protocolState) imageWrite(w wire) (event, error) {
	args, err := decodeWire(w.data(3))
	if err != nil {
		return event{}, err
	}
	location := string(args.data(1))
	data := args.data(5)
	valid := strings.HasPrefix(location, "/workspace/assets/") && path.Dir(location) == "/workspace/assets" && !strings.Contains(location, "\\") && len(args.data(2)) == 0 && len(data) > 0 && len(data) <= 16<<20 && strings.HasPrefix(http.DetectContentType(data), "image/")
	if !valid {
		return event{Replies: [][]byte{execReply(w, 3, bytesField(5, fields(textField(1, location), textField(2, nativePolicy))))}}, nil
	}
	if p.imageFiles == nil {
		p.imageFiles = make(map[string][]byte)
	}
	total := len(data)
	for _, v := range p.imageFiles {
		total += len(v)
	}
	if total > 64<<20 || len(p.imageFiles) >= 4096 {
		return event{}, reject(502, "image workspace budget exceeded")
	}
	if previous, exists := p.imageFiles[location]; exists && string(previous) != string(data) {
		location = fmt.Sprintf("/workspace/assets/%s-%s", randomID(), path.Base(location))
	}
	p.imageFiles[location] = append([]byte(nil), data...)
	success := fields(textField(1, location), numberField(3, uint64(len(data))))
	return event{Replies: [][]byte{execReply(w, 3, bytesField(1, success))}}, nil
}
