package redact

import (
	"strings"
	"testing"
)

func TestPathAndPatternRedaction(t *testing.T) {
	r, err := New([]byte("k"), Rules{Paths: []string{"params.arguments.password", "items.*.token"}}.WithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	in := map[string]any{
		"params": map[string]any{"arguments": map[string]any{"password": "hunter2", "user": "u-1"}},
		"items":  []any{map[string]any{"token": "t1"}, map[string]any{"token": "t2"}},
		"note":   "use sk-ABCDEFGHIJKLMNOPQRSTUV now",
	}
	out := r.Apply(in).(map[string]any)
	args := out["params"].(map[string]any)["arguments"].(map[string]any)
	if args["password"] == "hunter2" || args["user"] != "u-1" {
		t.Fatalf("path rule wrong: %v", args)
	}
	items := out["items"].([]any)
	if items[0].(map[string]any)["token"] == items[1].(map[string]any)["token"] {
		t.Fatal("different secrets got the same fingerprint")
	}
	if note := out["note"].(string); strings.Contains(note, "sk-") || !strings.HasPrefix(note, "use hmac:") {
		t.Fatalf("pattern rule wrong: %q", note)
	}
	if in["note"] != "use sk-ABCDEFGHIJKLMNOPQRSTUV now" {
		t.Fatal("Apply modified its input")
	}
}

func TestFingerprintsAreNotRedactedTwice(t *testing.T) {
	r, _ := New([]byte("k"), Rules{Paths: []string{"password"}})
	once := r.Apply(map[string]any{"password": "hunter2"}).(map[string]any)["password"]
	twice := r.Apply(map[string]any{"password": once}).(map[string]any)["password"]
	if once != twice {
		t.Fatalf("redacting a fingerprint changed it: %v -> %v", once, twice)
	}
}
