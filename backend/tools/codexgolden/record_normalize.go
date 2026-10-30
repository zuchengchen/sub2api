//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// normalizeRecords substitutes fixture endpoints and the two CLI-generated
// context blocks. Arbitrary user text, tool arguments/results and JSON strings
// are never passed through a global replacement. sjson retains wire key order.
func normalizeRecords(recs []record, sb *sandbox, port int) ([]record, error) {
	out := make([]record, len(recs))
	for i, rec := range recs {
		n := rec
		if rec.TLS != nil {
			hello := *rec.TLS
			if hello.ServerName == "localhost" || (rec.Hop == "gateway_upstream" && (hello.ServerName == "chatgpt.com" || hello.ServerName == "auth.openai.com")) {
				hello.ServerName = "<UPSTREAM>"
			}
			n.TLS = &hello
		}
		n.Headers = append([]headerKV(nil), rec.Headers...)
		for j, h := range n.Headers {
			if strings.EqualFold(h.Name, "host") && (h.Value == fmt.Sprintf("localhost:%d", port) || h.Value == fmt.Sprintf("127.0.0.1:%d", port)) {
				n.Headers[j].Value = "<UPSTREAM>"
			}
		}
		if len(rec.BodyJSON) > 0 {
			if !json.Valid(rec.BodyJSON) {
				return nil, fmt.Errorf("record %d: invalid body JSON", rec.Seq)
			}
			n.BodyJSON = append(json.RawMessage(nil), rec.BodyJSON...)
			input := gjson.GetBytes(rec.BodyJSON, "input").Array()
			for k, item := range input {
				text := item.Get("content.0.text").String()
				replacement := text
				// The fixture prompt is fixed, so the generated environment is
				// identifiable by its immediate successor. Lookalike text in
				// tool results or ordinary prompts must remain byte-for-byte.
				if item.Get("role").String() == "user" && strings.HasPrefix(text, "<environment_context>\n") && strings.HasSuffix(text, "\n</environment_context>") && k+1 < len(input) && input[k+1].Get("role").String() == "user" && input[k+1].Get("content.0.text").String() == "Reply with ok." {
					replacement = strings.ReplaceAll(replacement, "<cwd>"+sb.cwd+"</cwd>", "<cwd><CWD></cwd>")
					replacement = strings.ReplaceAll(replacement, "<root>"+sb.cwd+"</root>", "<root><CWD></root>")
				}
				if item.Get("role").String() == "developer" && strings.HasPrefix(text, "<skills_instructions>\n") && strings.Contains(text, "\n### Skill roots\n") {
					replacement = strings.ReplaceAll(replacement, "\n- `r0` = `"+sb.codexHome+"/skills/.system`\n", "\n- `r0` = `<CODEX_HOME>/skills/.system`\n")
				}
				if replacement != text {
					var err error
					n.BodyJSON, err = sjson.SetBytes(n.BodyJSON, fmt.Sprintf("input.%d.content.0.text", k), replacement)
					if err != nil {
						return nil, fmt.Errorf("record %d: normalize context: %w", rec.Seq, err)
					}
				}
			}
		}
		out[i] = n
	}
	return out, nil
}
