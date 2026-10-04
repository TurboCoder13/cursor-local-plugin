package provider

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"path"
	"strings"
	"unicode/utf8"
)

type attachment struct {
	Name, MIME, Text string
	Data             []byte
}
type content struct {
	Text        string
	Attachments []attachment
}
type contentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	ImageURL json.RawMessage `json:"image_url"`
	File     *filePart       `json:"file"`
	filePart
}
type filePart struct {
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
	FileID   string `json:"file_id"`
}

func inlineData(source string) (string, []byte, error) {
	media := "application/octet-stream"
	encoded := source
	isBase64 := true
	if strings.HasPrefix(source, "data:") {
		meta, payload, found := strings.Cut(source[5:], ",")
		if !found {
			return "", nil, reject(400, "invalid inline attachment")
		}
		encoded = payload
		isBase64 = strings.HasSuffix(meta, ";base64")
		media = strings.TrimSuffix(meta, ";base64")
		if media == "" {
			media = "text/plain"
		}
		var err error
		media, _, err = mime.ParseMediaType(media)
		if err != nil {
			return "", nil, reject(400, "invalid attachment MIME type")
		}
	}
	if len(encoded) > 24<<20 {
		return "", nil, reject(413, "attachment exceeds 16 MiB")
	}
	var data []byte
	var err error
	if isBase64 {
		data, err = base64.StdEncoding.DecodeString(encoded)
	} else {
		var decoded string
		decoded, err = url.PathUnescape(encoded)
		data = []byte(decoded)
	}
	if err != nil || len(data) > 16<<20 {
		return "", nil, reject(400, "invalid or oversized attachment encoding")
	}
	return media, data, nil
}

func parseContent(raw json.RawMessage) (content, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return content{}, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return content{Text: text}, nil
	}
	var parts []contentPart
	if json.Unmarshal(raw, &parts) != nil {
		return content{}, reject(400, "content must be text or an array of parts")
	}
	var out content
	var texts []string
	for i, part := range parts {
		if part.Type == "text" || part.Type == "input_text" || part.Type == "output_text" {
			texts = append(texts, part.Text)
			continue
		}
		var a attachment
		var source string
		isImage := part.Type == "image_url" || part.Type == "input_image"
		if isImage {
			if json.Unmarshal(part.ImageURL, &source) != nil {
				var u struct{ URL string }
				if json.Unmarshal(part.ImageURL, &u) != nil {
					return out, reject(400, "invalid image URL")
				}
				source = u.URL
			}
			if !strings.HasPrefix(source, "data:") {
				return out, reject(400, "images require inline data URLs; remote URLs are unavailable")
			}
			a.Name = fmt.Sprintf("image-%d", i+1)
		} else if part.Type == "file" || part.Type == "input_file" {
			f := part.filePart
			if part.File != nil {
				f = *part.File
			}
			if f.FileData == "" {
				return out, reject(400, "file_data is required; file_id cannot be resolved")
			}
			source = f.FileData
			a.Name = path.Base(strings.ReplaceAll(f.Filename, "\\", "/"))
			if a.Name == "." || a.Name == "/" || a.Name == "" {
				a.Name = "attachment"
			}
		} else {
			return out, reject(400, "unsupported content part")
		}
		media, data, err := inlineData(source)
		if err != nil {
			return out, err
		}
		a.MIME = media
		if strings.HasPrefix(media, "image/") {
			a.Data = data
		} else {
			if isImage || !utf8.Valid(data) {
				return out, reject(400, "attachment must be an image or UTF-8 text")
			}
			a.Text = string(data)
		}
		out.Attachments = append(out.Attachments, a)
		texts = append(texts, "[Attached: "+a.Name+"]")
	}
	out.Text = strings.Join(texts, "\n")
	return out, nil
}

func selectedContext(attachments []attachment) []byte {
	var selected []byte
	for _, a := range attachments {
		if a.Data != nil {
			image := fields(textField(2, randomID()), textField(3, a.Name), textField(7, a.MIME), bytesField(8, a.Data))
			selected = append(selected, bytesField(1, image)...)
		} else {
			selected = append(selected, bytesField(4, fields(textField(1, a.Text), textField(2, a.Name), textField(3, a.Name)))...)
		}
	}
	return selected
}
