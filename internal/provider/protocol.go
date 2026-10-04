package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// These wire numbers were checked against the installed official Cursor CLI
// 2026.09.02-c22c1a3. No descriptor or implementation from omsub is embedded.
const clientToolProvider = "cursor-local-client"

type atom struct {
	data []byte
	num  uint64
	kind protowire.Type
}
type wire map[int][]atom

func decodeWire(data []byte) (wire, error) {
	result := make(wire)
	count := 0
	for len(data) > 0 {
		count++
		if count > 65536 {
			return nil, malformedProtocol()
		}
		number, kind, n := protowire.ConsumeTag(data)
		if n < 0 || number <= 0 {
			return nil, malformedProtocol()
		}
		data = data[n:]
		value := atom{kind: kind}
		switch kind {
		case protowire.BytesType:
			value.data, n = protowire.ConsumeBytes(data)
		case protowire.VarintType:
			value.num, n = protowire.ConsumeVarint(data)
		case protowire.Fixed32Type:
			_, n = protowire.ConsumeFixed32(data)
		case protowire.Fixed64Type:
			_, n = protowire.ConsumeFixed64(data)
		default:
			return nil, malformedProtocol()
		}
		if n < 0 {
			return nil, malformedProtocol()
		}
		if len(result[int(number)]) >= 8192 {
			return nil, malformedProtocol()
		}
		result[int(number)] = append(result[int(number)], value)
		data = data[n:]
	}
	return result, nil
}

func (w wire) data(number int) []byte {
	values := w[number]
	if len(values) == 0 || values[len(values)-1].kind != protowire.BytesType {
		return nil
	}
	return values[len(values)-1].data
}

func (w wire) num(number int) uint64 {
	values := w[number]
	if len(values) == 0 {
		return 0
	}
	return values[len(values)-1].num
}

func bytesField(number int, data []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(nil, protowire.Number(number), protowire.BytesType), data)
}

func textField(number int, text string) []byte { return bytesField(number, []byte(text)) }

func numberField(number int, value uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(nil, protowire.Number(number), protowire.VarintType), value)
}

func fields(parts ...[]byte) []byte {
	var result []byte
	for _, part := range parts {
		result = append(result, part...)
	}
	return result
}

func requestContext() []byte {
	// This is a virtual environment, never a path on the gateway computer.
	env := fields(textField(1, "remote-client"), textField(2, "/workspace"), textField(10, "UTC"), textField(11, "/workspace"))
	return bytesField(4, env)
}

func contextWithTools(definitions [][]byte) []byte {
	ctx := requestContext()
	for _, definition := range definitions {
		ctx = append(ctx, bytesField(7, definition)...)
	}
	return ctx
}

func toolDefinitions(c chatRequest) ([][]byte, error) {
	var result [][]byte
	for _, t := range c.Tools {
		var schema any
		if json.Unmarshal(t.Function.Parameters, &schema) != nil {
			return nil, reject(400, "invalid tool schema")
		}
		value, err := structpb.NewValue(schema)
		if err != nil {
			return nil, reject(400, "invalid tool schema value")
		}
		encoded, err := proto.Marshal(value)
		if err != nil {
			return nil, reject(400, "cannot encode tool schema")
		}
		result = append(result, fields(textField(1, t.Function.Name), textField(2, t.Function.Description),
			bytesField(3, encoded), textField(4, clientToolProvider), textField(5, t.Function.Name), textField(6, string(t.Function.Parameters))))
	}
	return result, nil
}

func runPacket(c chatRequest) ([]byte, error) {
	definitions, err := toolDefinitions(c)
	if err != nil {
		return nil, err
	}
	user := fields(textField(1, c.Prompt), textField(2, randomID()))
	if len(c.Attachments) > 0 {
		user = append(user, bytesField(3, selectedContext(c.Attachments))...)
	}
	action := bytesField(1, fields(bytesField(1, user), bytesField(2, contextWithTools(definitions))))
	model := fields(textField(1, c.Model), textField(3, c.Model), textField(4, c.Model), textField(5, c.Model))
	var tools []byte
	for _, definition := range definitions {
		tools = append(tools, bytesField(1, definition)...)
	}
	run := fields(bytesField(1, c.Checkpoint), bytesField(2, action), bytesField(3, model), bytesField(4, tools),
		textField(5, c.ConversationID), numberField(19, 1))
	return bytesField(1, run), nil
}

type event struct {
	Text         string
	Call         *toolCall
	Done         bool
	Replies      [][]byte
	Image        string
	Checkpoint   []byte
	Thinking     bool
	ThinkingText string
	Interaction  bool
}

type protocolState struct {
	blobs      map[string][]byte
	bytes      int
	ids        map[string]string
	emitted    map[string]bool
	imageFiles map[string][]byte
}

func (p *protocolState) receive(raw []byte, c chatRequest) (event, error) {
	w, err := decodeWire(raw)
	if err != nil {
		return event{}, err
	}
	if data, exists := w[1]; exists {
		if len(data) != 1 || data[0].kind != protowire.BytesType {
			return event{}, malformedProtocol()
		}
		update, err := decodeWire(w.data(1))
		if err != nil {
			return event{}, err
		}
		if _, exists := update[3]; exists {
			return p.completed(update.data(3), c)
		}
		if _, exists := update[4]; exists {
			delta, err := decodeWire(update.data(4))
			if err != nil || !utf8.Valid(delta.data(1)) {
				return event{}, malformedProtocol()
			}
			return event{Thinking: true, ThinkingText: string(delta.data(1))}, nil
		}
		if _, exists := update[8]; exists {
			return event{Thinking: true}, nil
		}
		if _, exists := update[1]; exists {
			delta, err := decodeWire(update.data(1))
			text := string(delta.data(1))
			if err != nil || !utf8.ValidString(text) {
				return event{}, malformedProtocol()
			}
			return event{Text: text}, nil
		}
		_, done := update[14]
		return event{Done: done}, nil
	}
	if _, exists := w[2]; exists {
		return p.execution(w.data(2), c)
	}
	if _, exists := w[4]; exists {
		reply, err := p.blob(w.data(4))
		return event{Replies: [][]byte{reply}}, err
	}
	if _, exists := w[7]; exists {
		reply, err := interactionRefusal(w.data(7))
		return event{Replies: [][]byte{reply}, Interaction: true}, err
	}
	if _, exists := w[3]; exists {
		return event{Checkpoint: append([]byte(nil), w.data(3)...)}, nil
	}
	// Control notifications and timing are optional.
	for number := range w {
		if number != 3 && number != 5 && number != 8 {
			return event{}, malformedProtocol()
		}
	}
	return event{}, nil
}

func (p *protocolState) execution(raw []byte, c chatRequest) (event, error) {
	w, err := decodeWire(raw)
	if err != nil {
		return event{}, err
	}
	correlation := fields(numberField(1, w.num(1)), bytesField(15, w.data(15)))
	if _, exists := w[11]; exists {
		args, err := decodeWire(w.data(11))
		if err != nil {
			return event{}, err
		}
		rawID := string(args.data(3))
		if rawID == "" {
			rawID = fmt.Sprintf("exec_%d", w.num(1))
		}
		return p.mcp(args, c, rawID)
	}
	if _, exists := w[10]; exists {
		definitions, err := toolDefinitions(c)
		if err != nil {
			return event{}, err
		}
		result := bytesField(1, bytesField(1, contextWithTools(definitions)))
		return event{Replies: [][]byte{bytesField(2, fields(correlation, bytesField(10, result)))}}, nil
	}
	if _, exists := w[36]; exists {
		definitions, err := toolDefinitions(c)
		if err != nil {
			return event{}, err
		}
		server := fields(textField(1, clientToolProvider), textField(2, clientToolProvider))
		for _, definition := range definitions {
			server = append(server, bytesField(5, definition)...)
		}
		result := bytesField(1, bytesField(1, server))
		return event{Replies: [][]byte{bytesField(2, fields(correlation, bytesField(36, result)))}}, nil
	}
	if _, exists := w[3]; exists {
		return p.imageWrite(w)
	}
	for _, operation := range []int{2, 14} {
		if _, exists := w[operation]; exists {
			return shellRefusal(w, operation)
		}
	}
	for _, number := range []int{5, 7, 29} {
		if _, exists := w[number]; exists {
			failure := textField(1, nativePolicy)
			if number != 5 {
				args, err := decodeWire(w.data(number))
				if err != nil {
					return event{}, err
				}
				failure = fields(textField(1, string(args.data(1))), textField(2, nativePolicy))
			}
			return event{Replies: [][]byte{bytesField(2, fields(correlation, bytesField(number, bytesField(2, failure))))}}, nil
		}
	}

	for number := range w {
		if number != 1 && number != 15 && number != 19 && number != 57 && number != 55 {
			return event{}, unsupportedOperation(number)
		}
	}
	return event{}, malformedProtocol()
}

func (p *protocolState) blob(raw []byte) ([]byte, error) {
	w, err := decodeWire(raw)
	if err != nil {
		return nil, err
	}
	var result []byte
	if _, exists := w[2]; exists {
		args, err := decodeWire(w.data(2))
		if err != nil {
			return nil, err
		}
		result = bytesField(2, bytesField(1, p.blobs[string(args.data(1))]))
	} else if _, exists := w[3]; exists {
		args, err := decodeWire(w.data(3))
		if err != nil {
			return nil, err
		}
		if p.blobs == nil {
			p.blobs = make(map[string][]byte)
		}
		key, data := string(args.data(1)), args.data(2)
		prior, exists := p.blobs[key]
		projected := p.bytes - len(prior) + len(data)
		if !exists {
			projected += len(key)
		}
		if len(data) > 16<<20 || len(key) > 4096 || projected > 64<<20 || (!exists && len(p.blobs) >= 4096) {
			return nil, reject(502, "Cursor in-memory blob budget exceeded")
		}
		p.blobs[key] = append([]byte(nil), data...)
		p.bytes = projected
		result = bytesField(3, nil)
	} else {
		return nil, malformedProtocol()
	}
	return bytesField(3, fields(numberField(1, w.num(1)), result)), nil
}

func interactionRefusal(raw []byte) ([]byte, error) {
	w, err := decodeWire(raw)
	if err != nil {
		return nil, err
	}
	policy := textField(1, "This API gateway cannot approve native interactions. Use the client function tools.")
	for number := range w {
		var result []byte
		switch number {
		case 1:
			continue
		case 12:
			query, err := decodeWire(w.data(12))
			if err != nil {
				return nil, err
			}
			args, err := decodeWire(query.data(1))
			if err != nil {
				return nil, err
			}
			description := string(args.data(1))
			if strings.TrimSpace(description) == "" {
				return nil, reject(502, "image generation requires a description")
			}
			result = bytesField(1, textField(1, description))
		case 2, 4, 5, 6, 9:
			result = bytesField(2, policy)
		case 3:
			result = bytesField(1, bytesField(3, policy))
		case 7:
			result = bytesField(1, bytesField(2, policy))
		default:
			return nil, reject(502, fmt.Sprintf("unsupported Cursor interaction %d", number))
		}
		return bytesField(6, fields(numberField(1, w.num(1)), bytesField(number, result))), nil
	}
	return nil, malformedProtocol()
}
