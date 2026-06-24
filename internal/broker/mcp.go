package broker

import "encoding/json"

// parseJSONRPC decodes a JSON-RPC message and returns its method (if any).
func parseJSONRPC(body []byte) (string, map[string]any, bool) {
	var msg map[string]any
	if err := json.Unmarshal(body, &msg); err != nil {
		return "", nil, false
	}
	method, _ := msg["method"].(string)
	return method, msg, true
}

// toolsCallFields extracts a tools/call's tool name, string arguments (excluding
// _capability), and the _capability token. ok is false if the shape is wrong.
func toolsCallFields(msg map[string]any) (string, map[string]string, string, bool) {
	params, ok := msg["params"].(map[string]any)
	if !ok {
		return "", nil, "", false
	}
	name, _ := params["name"].(string)
	rawArgs, _ := params["arguments"].(map[string]any)
	args := map[string]string{}
	capability := ""
	for k, v := range rawArgs {
		s, _ := v.(string)
		if k == "_capability" {
			capability = s
			continue
		}
		args[k] = s
	}
	if name == "" {
		return "", nil, "", false
	}
	return name, args, capability, true
}

// stripCapability re-marshals the message with params.arguments._capability
// removed, so the token never reaches the upstream tool.
func stripCapability(msg map[string]any) []byte {
	if params, ok := msg["params"].(map[string]any); ok {
		if args, ok := params["arguments"].(map[string]any); ok {
			delete(args, "_capability")
		}
	}
	out, _ := json.Marshal(msg)
	return out
}

// augmentToolsListSchema injects a `_capability` string property into every
// advertised tool's inputSchema.properties, so the MCP client includes the
// token on calls.
func augmentToolsListSchema(respBody []byte) []byte {
	var msg map[string]any
	if err := json.Unmarshal(respBody, &msg); err != nil {
		return respBody // pass through unparseable bodies unchanged
	}
	result, ok := msg["result"].(map[string]any)
	if !ok {
		return respBody
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		return respBody
	}
	for _, ti := range tools {
		tool, ok := ti.(map[string]any)
		if !ok {
			continue
		}
		schema, ok := tool["inputSchema"].(map[string]any)
		if !ok {
			schema = map[string]any{"type": "object"}
			tool["inputSchema"] = schema
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			props = map[string]any{}
			schema["properties"] = props
		}
		props["_capability"] = map[string]any{
			"type":        "string",
			"description": "lever capability token authorizing this call",
		}
	}
	out, err := json.Marshal(msg)
	if err != nil {
		return respBody
	}
	return out
}
