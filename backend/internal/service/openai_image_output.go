package service

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // Inspect animated output without decoding or changing its frames.
	"image/jpeg"
	"image/png"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

const openAIImageOutputRequestKey = "openai_image_output_request"

// This state contains only image options, never prompts or credentials. It is
// replaced for every forwarding attempt / WebSocket turn.
type openAIImageOutputRequest struct {
	Model, Size, Format, Background string
	mu                              sync.Mutex
	reportedModel                   string
}

func setOpenAIImageOutputRequest(c *gin.Context, body []byte) {
	if c == nil {
		return
	}
	var options *openAIImageOutputRequest
	read := func(value gjson.Result) {
		options = &openAIImageOutputRequest{
			Model: value.Get("model").String(), Size: value.Get("size").String(),
			Format: value.Get("output_format").String(), Background: value.Get("background").String(),
		}
	}
	root := gjson.ParseBytes(body)
	if IsGPTImageGenerationModel(root.Get("model").String()) {
		read(root)
	}
	declaredTools := root.Get("tools").Array()
	for _, item := range root.Get("input").Array() {
		if item.Get("type").String() == "additional_tools" {
			declaredTools = append(declaredTools, item.Get("tools").Array()...)
		}
	}
	for _, tool := range declaredTools {
		if tool.Get("type").String() == "image_generation" {
			previous := options
			read(tool)
			if previous != nil {
				if options.Model == "" {
					options.Model = previous.Model
				}
				if options.Size == "" {
					options.Size = previous.Size
				}
				if options.Format == "" {
					options.Format = previous.Format
				}
				if options.Background == "" {
					options.Background = previous.Background
				}
			}
			break
		}
	}
	c.Set(openAIImageOutputRequestKey, options)
}

func openAIImageOutputRequestFromContext(c *gin.Context) *openAIImageOutputRequest {
	if c == nil {
		return nil
	}
	value, _ := c.Get(openAIImageOutputRequestKey)
	options, _ := value.(*openAIImageOutputRequest)
	return options
}

type openAIImageOutputMetadata struct {
	RequestedModel string   `json:"requested_model,omitempty"`
	ReportedModel  string   `json:"reported_model,omitempty"`
	ModelStatus    string   `json:"model_status"`
	RequestedSize  string   `json:"requested_size,omitempty"`
	UpstreamSize   string   `json:"upstream_size,omitempty"`
	OutputSize     string   `json:"output_size,omitempty"`
	Postprocessed  bool     `json:"postprocessed"`
	ResizeMode     string   `json:"resize_mode,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
}

func observeOpenAIImageOutputModel(c *gin.Context, payload []byte) {
	options := openAIImageOutputRequestFromContext(c)
	if options == nil {
		return
	}
	root := gjson.ParseBytes(payload)
	if nested := root.Get("response"); nested.IsObject() {
		root = nested
	}
	for _, tool := range root.Get("tools").Array() {
		if tool.Get("type").String() == "image_generation" {
			if model := strings.TrimSpace(tool.Get("model").String()); model != "" {
				options.mu.Lock()
				options.reportedModel = model
				options.mu.Unlock()
			}
			break
		}
	}
}

// prepareOpenAIImageOutput changes only the downstream copy, after usage,
// replay and upstream-model observation. It never changes a tool's model or
// manufactures an animation, and preserves all unrelated response fields.
func prepareOpenAIImageOutput(c *gin.Context, body []byte) []byte {
	options := openAIImageOutputRequestFromContext(c)
	if options == nil || !gjson.ValidBytes(body) {
		return body
	}
	observeOpenAIImageOutputModel(c, body)
	options.mu.Lock()
	defer options.mu.Unlock()
	root := gjson.ParseBytes(body)
	update := func(path, imageField string, item gjson.Result, reported string) {
		if model := strings.TrimSpace(item.Get("model").String()); model != "" {
			reported = model
		}
		meta := openAIImageOutputMetadata{RequestedModel: options.Model, ReportedModel: reported, RequestedSize: options.Size, ModelStatus: "unreported"}
		if reported != "" {
			meta.ModelStatus = "reported"
			if expected := resolveOpenAIImageModelAlias(options.Model); expected != "" && !strings.EqualFold(expected, reported) {
				meta.ModelStatus = "different"
				meta.Warnings = append(meta.Warnings, "upstream_image_model_differs")
			}
		}
		encoded := item.Get(imageField).String()
		if encoded == "" {
			return
		}
		if imageField == "url" && !strings.HasPrefix(encoded, "data:image/") {
			meta.Warnings = append(meta.Warnings, "remote_image_not_processed")
			body, _ = sjson.SetBytes(body, path+"sub2api_image", meta)
			return
		}
		if strings.HasPrefix(encoded, "data:image/") {
			_, encoded, _ = strings.Cut(encoded, ",")
		}
		// Bound both compressed input and decoded dimensions before allocation.
		if len(encoded) > 48<<20 {
			meta.Warnings = append(meta.Warnings, "image_postprocessing_input_limit")
			body, _ = sjson.SetBytes(body, path+"sub2api_image", meta)
			return
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			raw, err = base64.RawStdEncoding.DecodeString(encoded)
		}
		if err == nil {
			cfg, format, configErr := image.DecodeConfig(bytes.NewReader(raw))
			if configErr == nil && cfg.Width > 0 && cfg.Height > 0 {
				actualSize := fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
				meta.UpstreamSize, meta.OutputSize = actualSize, actualSize
				width, height, exact := openAIImageOutputDimensions(options.Size)
				if exact && (width != cfg.Width || height != cfg.Height) {
					if (format == "png" || format == "jpeg") && cfg.Width <= 8192 && cfg.Height <= 8192 && int64(cfg.Width)*int64(cfg.Height) <= 16_777_216 {
						resized, resizeErr := resizeOpenAIImageOutput(raw, format, width, height, options.Background)
						if resizeErr == nil {
							value := base64.StdEncoding.EncodeToString(resized)
							if imageField == "url" {
								value = "data:image/" + format + ";base64," + value
							}
							body, _ = sjson.SetBytes(body, path+imageField, value)
							meta.OutputSize, meta.Postprocessed, meta.ResizeMode = options.Size, true, "contain"
						} else {
							meta.Warnings = append(meta.Warnings, "image_resize_failed")
						}
					} else {
						meta.Warnings = append(meta.Warnings, "image_resize_unavailable")
					}
				}
				body, _ = sjson.SetBytes(body, path+"size", meta.OutputSize)
				// Describe the actual bytes, even if the upstream metadata is wrong.
				body, _ = sjson.SetBytes(body, path+"output_format", format)
				// Some streaming adapters expose both fields. Keep the URL bytes
				// and MIME consistent with the final b64_json, including no-resize
				// cases where the upstream mislabeled the original data URI.
				if strings.HasPrefix(item.Get("url").String(), "data:image/") {
					value := gjson.GetBytes(body, path+imageField).String()
					if strings.HasPrefix(value, "data:") {
						_, value, _ = strings.Cut(value, ",")
					}
					body, _ = sjson.SetBytes(body, path+"url", "data:image/"+format+";base64,"+value)
				}
				if options.Format != "" && !strings.EqualFold(options.Format, format) {
					meta.Warnings = append(meta.Warnings, "upstream_image_format_differs")
				}
			} else {
				meta.Warnings = append(meta.Warnings, "image_format_unverified")
			}
		} else {
			meta.Warnings = append(meta.Warnings, "image_base64_invalid")
		}
		body, _ = sjson.SetBytes(body, path+"sub2api_image", meta)
	}
	switch root.Get("type").String() {
	case "response.output_item.done":
		item := root.Get("item")
		if item.Get("type").String() == "image_generation_call" {
			update("item.", "result", item, options.reportedModel)
		}
	case "image_generation.completed", "image_edit.completed":
		field := "b64_json"
		if !root.Get(field).Exists() && root.Get("url").String() != "" {
			field = "url"
		}
		update("", field, root, root.Get("model").String())
	default:
		for _, prefix := range []string{"", "response."} {
			for i, item := range root.Get(prefix + "output").Array() {
				if item.Get("type").String() == "image_generation_call" {
					update(fmt.Sprintf("%soutput.%d.", prefix, i), "result", item, options.reportedModel)
				}
			}
		}
		for i, item := range root.Get("data").Array() {
			field := "b64_json"
			if !item.Get(field).Exists() && item.Get("url").String() != "" {
				field = "url"
			}
			update(fmt.Sprintf("data.%d.", i), field, item, root.Get("model").String())
		}
		if size := gjson.GetBytes(body, "data.0.size").String(); size != "" {
			body, _ = sjson.SetBytes(body, "size", size)
		}
		if format := gjson.GetBytes(body, "data.0.output_format").String(); format != "" {
			body, _ = sjson.SetBytes(body, "output_format", format)
		}
	}
	return compactOpenAIImageOutputJSON(body)
}

func compactOpenAIImageOutputJSON(body []byte) []byte {
	if bytes.ContainsAny(body, "\r\n") {
		var compact bytes.Buffer
		if json.Compact(&compact, body) == nil {
			return compact.Bytes()
		}
	}
	return body
}

func openAIImageOutputDimensions(size string) (int, int, bool) {
	w, h, ok := strings.Cut(strings.TrimSpace(size), "x")
	if !ok {
		return 0, 0, false
	}
	width, werr := strconv.Atoi(w)
	height, herr := strconv.Atoi(h)
	return width, height, werr == nil && herr == nil && width > 0 && height > 0 && width <= 3840 && height <= 3840 && int64(width)*int64(height) <= 8_294_400
}

// Keep SSE event names, IDs, comments and retry fields while replacing a
// complete (possibly multi-line) JSON data payload. Never modify usage input.
func prepareOpenAIImageSSEOutput(c *gin.Context, frame []byte) []byte {
	lines := strings.Split(string(frame), "\n")
	var data []string
	for _, line := range lines {
		if value, ok := extractOpenAISSEDataLine(strings.TrimSuffix(line, "\r")); ok {
			data = append(data, value)
		}
	}
	input := []byte(strings.Join(data, "\n"))
	output := prepareOpenAIImageOutput(c, input)
	if bytes.Equal(input, output) {
		return frame
	}
	var result strings.Builder
	written := false
	for i, line := range lines {
		if _, ok := extractOpenAISSEDataLine(strings.TrimSuffix(line, "\r")); ok {
			if written {
				continue
			}
			line = "data: " + string(output)
			written = true
		}
		result.WriteString(line)
		if i < len(lines)-1 {
			result.WriteByte('\n')
		}
	}
	return []byte(result.String())
}

func resizeOpenAIImageOutput(raw []byte, format string, width, height int, background string) ([]byte, error) {
	if _, _, valid := openAIImageOutputDimensions(fmt.Sprintf("%dx%d", width, height)); !valid || (format != "png" && format != "jpeg") {
		return nil, fmt.Errorf("unsupported image resize target")
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	if format == "jpeg" || background == "opaque" {
		draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	}
	// Preserve the whole image and its aspect ratio. Pad only when needed.
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	w, h := width, height
	if int64(width)*int64(sh) <= int64(height)*int64(sw) {
		h = max(1, sh*width/sw)
	} else {
		w = max(1, sw*height/sh)
	}
	x, y := (width-w)/2, (height-h)/2
	draw.CatmullRom.Scale(dst, image.Rect(x, y, x+w, y+h), src, src.Bounds(), draw.Over, nil)
	var output bytes.Buffer
	if format == "jpeg" {
		err = jpeg.Encode(&output, dst, &jpeg.Options{Quality: 95})
	} else {
		err = png.Encode(&output, dst)
	}
	return output.Bytes(), err
}
