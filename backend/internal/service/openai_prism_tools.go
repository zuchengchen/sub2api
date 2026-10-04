package service

import (
	"bytes"
	"errors"

	"github.com/tidwall/gjson"
)

func prismBrowserValidateToolCatalog(request, response []byte, stream bool) error {
	terminal := response
	if stream {
		for _, line := range bytes.Split(response, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
				if gjson.GetBytes(data, "type").String() == "response.completed" {
					terminal = []byte(gjson.GetBytes(data, "response").Raw)
				}
			}
		}
	}
	catalog := make(map[string]string)
	var collect func(gjson.Result, string, int)
	collect = func(value gjson.Result, namespace string, depth int) {
		if depth > 8 || !value.IsArray() {
			return
		}
		for _, tool := range value.Array() {
			kind, name := tool.Get("type").String(), tool.Get("name").String()
			if kind == "namespace" {
				nested := name
				if namespace != "" {
					nested = namespace + "." + name
				}
				collect(tool.Get("tools"), nested, depth+1)
			} else if name != "" && (kind == "function" || kind == "custom") {
				catalog[namespace+"\x00"+name] = kind
			}
		}
	}
	collect(gjson.GetBytes(request, "tools"), "", 0)
	collect(gjson.GetBytes(request, "additional_tools"), "", 0)
	for _, item := range gjson.GetBytes(request, "input").Array() {
		if item.Get("type").String() == "additional_tools" {
			collect(item.Get("tools"), "", 0)
		}
	}
	for _, item := range gjson.GetBytes(terminal, "output").Array() {
		kind := item.Get("type").String()
		if kind != "function_call" && kind != "custom_tool_call" {
			continue
		}
		expected := "function"
		if kind == "custom_tool_call" {
			expected = "custom"
		}
		if catalog[item.Get("namespace").String()+"\x00"+item.Get("name").String()] != expected || gjson.GetBytes(request, "tool_choice").String() == "none" {
			return errors.New("prism adapter returned a tool absent from the client catalog")
		}
	}
	return nil
}
