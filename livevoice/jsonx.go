package livevoice

import "encoding/json"

// The provider protocols are read as loosely as JavaScript read them: a field
// of the wrong type is simply absent.

func parseObject(data []byte) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]any{}
	}
	if m == nil {
		return map[string]any{}
	}
	return m
}

func obj(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func truthy(m map[string]any, key string) bool {
	switch v := m[key].(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	}
	return true
}

func list(m map[string]any, key string) []any {
	v, _ := m[key].([]any)
	return v
}
