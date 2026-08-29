package strictjson

import "testing"

func TestRejectDuplicateKeysAcceptsOneNestedJSONValue(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		`null`,
		`{"schema_version":1,"nested":{"value":true},"items":[{"name":"a"},{"name":"b"}]}`,
	} {
		if err := RejectDuplicateKeys([]byte(value)); err != nil {
			t.Fatalf("valid JSON %q rejected: %v", value, err)
		}
	}
}

func TestRejectDuplicateKeysRejectsDuplicatesMalformedAndTrailingData(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]string{
		"root duplicate":    `{"a":1,"a":2}`,
		"nested duplicate":  `{"nested":{"a":1,"a":2}}`,
		"array duplicate":   `[{"a":1,"a":2}]`,
		"escaped duplicate": `{"a":1,"\u0061":2}`,
		"malformed":         `{"a":`,
		"trailing":          `{"a":1}{"b":2}`,
	} {
		name, value := name, value
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := RejectDuplicateKeys([]byte(value)); err == nil {
				t.Fatal("invalid or duplicate-key JSON was accepted")
			}
		})
	}
}
