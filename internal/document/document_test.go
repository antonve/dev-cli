package document

import "testing"

func TestUnmarshal(t *testing.T) {
	for _, input := range []string{`{"camelCase":"hello","port":8000}`, "camelCase: hello\nport: 8000\n", "---\ncamelCase: hello\nport: 8000\n...\n"} {
		var got struct {
			Name string `json:"camelCase"`
			Port int    `json:"port"`
		}
		if err := Unmarshal([]byte(input), &got); err != nil || got.Name != "hello" || got.Port != 8000 {
			t.Fatalf("decode %q = %+v, %v", input, got, err)
		}
	}
}

func TestRejectAmbiguousDocuments(t *testing.T) {
	for _, input := range []string{
		"", "null", "[]", "true", "spec: [", "spec: {}\nspec: {}",
		`{"spec":{},"spec":{}}`, "spec: {}\n---\nkind: Secret", "spec: {}\n---\n", "spec: {}\n---\ninvalid: [",
	} {
		var got map[string]any
		if err := Unmarshal([]byte(input), &got); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}
