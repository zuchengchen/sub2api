package service

import (
	"bytes"
	"encoding/xml"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Bound time zone file lookups (including invalid names) and nested user
// metadata. Ordinary Responses and Chat message content fits within six levels.
const openAIClientClockMaxZones = 16
const openAIClientClockMaxDepth = 16

// A replayed history must describe the same instant on every request. Collect
// source fields only within the same JSON object or XML environment block.
func rewriteGPTClientLocalTimes(body []byte, timezone string) []byte {
	if (!bytes.Contains(body, []byte("timezone")) && !bytes.Contains(body, []byte("time_zone"))) || !gjson.ValidBytes(body) {
		return body
	}
	if !bytes.Contains(body, []byte("current_time")) {
		if !bytes.Contains(body, []byte("user_location")) {
			return body
		}
		next, _ := rewriteOpenAIUserLocationTimezones(body, timezone)
		return next
	}
	projection := openAIClientClockProjection{targetName: timezone}
	var edits []openAILocalTimeEdit
	var visit func(gjson.Result, int)
	visit = func(node gjson.Result, depth int) {
		if depth > openAIClientClockMaxDepth {
			return
		}
		if node.Type == gjson.String {
			if !strings.Contains(node.Raw, "environment_context") {
				return
			}
			// Only a standalone environment block represents client context.
			// Quoted XML in ordinary prose, code samples and tool output is data.
			trimmed := strings.TrimSpace(node.Str)
			if strings.HasPrefix(trimmed, "<environment_context>") && strings.HasSuffix(trimmed, "</environment_context>") {
				next := projection.environment(node.Str)
				if next != node.Str {
					edits = append(edits, openAILocalTimeEdit{node.Index, node.Index + len(node.Raw), mustJSONString(next)})
				}
			}
			return
		}
		if node.Type != gjson.JSON {
			return
		}
		if !strings.Contains(node.Raw, "timezone") && !strings.Contains(node.Raw, "time_zone") {
			return
		}
		if node.IsArray() {
			node.ForEach(func(_, item gjson.Result) bool {
				visit(item, depth+1)
				return true
			})
			return
		}
		switch node.Get("role").Str {
		case "assistant", "tool", "function":
			return
		}
		if strings.Contains(node.Raw, `"current_time"`) && node.Get("current_time").Type == gjson.String {
			// Process this object's clock before its children. Appending later
			// turns must not affect its place in the zone lookup budget.
			edits = projection.appendJSONClock(edits, node)
		}
		node.ForEach(func(key, value gjson.Result) bool {
			switch key.Str {
			case "input", "messages", "content", "text", "instructions", "metadata", "client_metadata", "environment_context", "response":
				visit(value, depth+1)
			}
			return true
		})
	}
	visit(parseRawJSONView(body), 0)
	next := body
	if len(edits) != 0 {
		slices.SortFunc(edits, func(a, b openAILocalTimeEdit) int { return a.start - b.start })
		var out bytes.Buffer
		out.Grow(len(body))
		end := 0
		for _, edit := range edits {
			out.Write(body[end:edit.start])
			out.WriteString(edit.value)
			end = edit.end
		}
		out.Write(body[end:])
		next = out.Bytes()
	}
	if bytes.Contains(next, []byte("user_location")) {
		next, _ = rewriteOpenAIUserLocationTimezones(next, timezone)
	}
	return next
}

type openAILocalTimeEdit struct {
	start, end int
	value      string
}

func openAILocalTimeField(name string) int {
	switch name {
	case "current_date", "local_date":
		return 0
	case "current_time":
		return 1
	case "timezone", "time_zone":
		return 2
	default:
		return -1
	}
}

type openAIClientClockProjection struct {
	targetName string
	target     *time.Location
	zones      map[string]*time.Location
}

func (p *openAIClientClockProjection) appendJSONClock(edits []openAILocalTimeEdit, node gjson.Result) []openAILocalTimeEdit {
	var fields [3]gjson.Result
	valid := true
	node.ForEach(func(key, value gjson.Result) bool {
		switch key.Str {
		case "current_date", "current_time", "timezone":
			index := openAILocalTimeField(key.Str)
			if fields[index].Exists() || value.Type != gjson.String {
				valid = false
				return false
			}
			fields[index] = value
		}
		return true
	})
	if !valid {
		return edits
	}
	clock, ok := p.convert(fields[0].Str, fields[1].Str, fields[2].Str)
	if !ok {
		return edits
	}
	for i, field := range fields {
		if clock[i] != field.Str {
			edits = append(edits, openAILocalTimeEdit{field.Index, field.Index + len(field.Raw), mustJSONString(clock[i])})
		}
	}
	return edits
}

func (p *openAIClientClockProjection) environment(block string) string {
	decoder := xml.NewDecoder(strings.NewReader(block))
	var fields [3]string
	var starts [3]int
	var order []int
	depth := 0
	rootSeen := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return block
		}
		switch token := token.(type) {
		case xml.StartElement:
			if depth >= openAIClientClockMaxDepth {
				return block
			}
			if depth == 0 {
				if rootSeen || token.Name.Local != "environment_context" {
					return block
				}
				rootSeen = true
			}
			index := openAILocalTimeField(token.Name.Local)
			if depth != 1 || index < 0 {
				depth++
				continue
			}
			if fields[index] != "" || token.Name.Space != "" || len(token.Attr) != 0 {
				return block
			}
			start := int(decoder.InputOffset())
			leaf, err := decoder.Token()
			if err != nil {
				return block
			}
			text, ok := leaf.(xml.CharData)
			if !ok || len(text) == 0 || len(text) > 256 {
				return block
			}
			value := string(text)
			end, err := decoder.Token()
			if err != nil || end != token.End() {
				return block
			}
			raw := block[start:int(decoder.InputOffset())]
			// A clock is a plain leaf. Nested markup and XML entities must not
			// supply an instant or shift the byte offsets of the original value.
			if raw != value+"</"+token.Name.Local+">" {
				return block
			}
			fields[index] = strings.TrimSpace(value)
			if fields[index] == "" {
				return block
			}
			starts[index] = start + strings.Index(value, fields[index])
			order = append(order, index)
		case xml.EndElement:
			depth--
		}
	}
	if len(order) != len(fields) {
		return block
	}
	clock, ok := p.convert(fields[0], fields[1], fields[2])
	if !ok {
		return block
	}
	var out strings.Builder
	end := 0
	for _, index := range order {
		offset := starts[index]
		out.WriteString(block[end:offset])
		out.WriteString(clock[index])
		end = offset + len(fields[index])
	}
	out.WriteString(block[end:])
	return out.String()
}

// ParseInLocation chooses an instant during a DST overlap and normalizes gaps.
// Round-trip the civil time and check the adjacent offsets to require a unique
// source instant, including transitions whose change is not a whole hour.
func (p *openAIClientClockProjection) convert(date, clock, timezone string) ([3]string, bool) {
	var none [3]string
	if (timezone != "UTC" && timezone != "GMT" && !strings.Contains(timezone, "/")) || timezone == p.targetName ||
		len(timezone) > 128 || len(date) != len(time.DateOnly) || len(clock) != len(time.TimeOnly) {
		return none, false
	}
	if p.zones == nil {
		p.target, _ = time.LoadLocation(p.targetName)
		p.zones = make(map[string]*time.Location)
	}
	if p.target == nil {
		return none, false
	}
	source, known := p.zones[timezone]
	if !known {
		if len(p.zones) >= openAIClientClockMaxZones {
			return none, false
		}
		source, _ = time.LoadLocation(timezone)
		p.zones[timezone] = source
	}
	if source == nil {
		return none, false
	}
	wall := date + " " + clock
	stamp, err := time.ParseInLocation(time.DateTime, wall, source)
	if err != nil || stamp.Format(time.DateTime) != wall {
		return none, false
	}
	_, offset := stamp.Zone()
	start, end := stamp.ZoneBounds()
	overlaps := func(boundary time.Time) bool {
		_, adjacent := boundary.In(source).Zone()
		return adjacent != offset && stamp.Add(time.Duration(offset-adjacent)*time.Second).Format(time.DateTime) == wall
	}
	if (!start.IsZero() && overlaps(start.Add(-time.Second))) || (!end.IsZero() && overlaps(end)) {
		return none, false
	}
	local := stamp.In(p.target)
	return [3]string{local.Format(time.DateOnly), local.Format(time.TimeOnly), p.target.String()}, true
}
