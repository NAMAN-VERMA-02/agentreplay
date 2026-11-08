package cassette

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NAMAN-VERMA-02/agentreplay/internal/redact"
)

func pipe(t *testing.T, rules redact.Rules) *Pipeline {
	t.Helper()
	p, err := NewPipeline([]byte("test-key"), rules.WithDefaults())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func hashOf(t *testing.T, p *Pipeline, raw string) string {
	t.Helper()
	out, err := p.Process([]byte(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	return out.Hash
}

func TestKeyOrderDoesNotChangeHash(t *testing.T) {
	p := pipe(t, redact.Rules{})
	a := hashOf(t, p, `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":5}`)
	b := hashOf(t, p, `{"max_tokens":5,"messages":[{"content":"hi","role":"user"}],"model":"m"}`)
	if a != b {
		t.Fatal("key order changed the hash")
	}
}

func TestMeaningfulFieldsStayInHash(t *testing.T) {
	p := pipe(t, redact.Rules{})
	base := `{"model":"m1","temperature":0,"metadata":{"user_id":"u-1"},"messages":[]}`
	for _, changed := range []string{
		`{"model":"m2","temperature":0,"metadata":{"user_id":"u-1"},"messages":[]}`,
		`{"model":"m1","temperature":1,"metadata":{"user_id":"u-1"},"messages":[]}`,
		`{"model":"m1","temperature":0,"metadata":{"user_id":"u-2"},"messages":[]}`,
	} {
		if hashOf(t, p, base) == hashOf(t, p, changed) {
			t.Errorf("hash ignored a meaningful change: %s", changed)
		}
	}
}

func TestIgnorePathsAreDropped(t *testing.T) {
	p := pipe(t, redact.Rules{Ignore: []string{"meta.sent_at"}})
	a := hashOf(t, p, `{"q":1,"meta":{"sent_at":"2026-01-01T00:00:00Z","trace":"x"}}`)
	b := hashOf(t, p, `{"q":1,"meta":{"sent_at":"2027-05-05T00:00:00Z","trace":"x"}}`)
	c := hashOf(t, p, `{"q":1,"meta":{"sent_at":"2027-05-05T00:00:00Z","trace":"y"}}`)
	if a != b {
		t.Fatal("ignored timestamp changed the hash")
	}
	if a == c {
		t.Fatal("a non-ignored sibling was dropped too")
	}
}

func TestSecretsNeverStoredButStillDistinguish(t *testing.T) {
	p := pipe(t, redact.Rules{Paths: []string{"params.arguments.password"}})
	raw1 := `{"params":{"arguments":{"password":"hunter2-very-secret","note":"key sk-ABCDEFGHIJKLMNOPQRSTUV"}}}`
	raw2 := `{"params":{"arguments":{"password":"another-secret-value","note":"key sk-ABCDEFGHIJKLMNOPQRSTUV"}}}`
	o1, _ := p.Process([]byte(raw1), nil)
	o2, _ := p.Process([]byte(raw2), nil)
	stored := string(Canonical(o1.Stored))
	for _, secret := range []string{"hunter2", "sk-ABCDEFGHIJKLMNOPQRSTUV"} {
		if strings.Contains(stored, secret) || strings.Contains(string(o1.Canonical), secret) {
			t.Errorf("secret %q reached the stored form: %s", secret, stored)
		}
	}
	if !strings.Contains(stored, "hmac:") {
		t.Fatal("no fingerprint in stored form")
	}
	if o1.Hash == o2.Hash {
		t.Fatal("requests differing only in a secret hashed the same")
	}
}

func TestWrongKeyChangesFingerprints(t *testing.T) {
	r1, _ := redact.New([]byte("k1"), redact.Rules{}.WithDefaults())
	r2, _ := redact.New([]byte("k2"), redact.Rules{}.WithDefaults())
	if r1.Fingerprint("x") == r2.Fingerprint("x") || redact.KeyCheck([]byte("k1")) == redact.KeyCheck([]byte("k2")) {
		t.Fatal("different keys gave the same fingerprint")
	}
	if _, err := redact.New(nil, redact.Rules{}); err == nil {
		t.Fatal("empty key accepted")
	}
}

func TestPoolNthGetsNth(t *testing.T) {
	evs := []*Event{{Seq: 1, RequestHash: "h"}, {Seq: 2, RequestHash: "other"}, {Seq: 3, RequestHash: "h"}}
	pool := NewPool(evs, func(e *Event) string { return e.RequestHash })
	a, _ := pool.Take("h")
	b, _ := pool.Take("h")
	if a.Seq != 1 || b.Seq != 3 {
		t.Fatalf("got seq %d then %d, want 1 then 3", a.Seq, b.Seq)
	}
	if _, ok := pool.Take("h"); ok {
		t.Fatal("pool handed out more than was recorded")
	}
	if u := pool.Unused(); len(u) != 1 || u[0].Seq != 2 {
		t.Fatalf("unused = %v", u)
	}
}

func TestWriterRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "base.jsonl")
	w, err := Create(path, true)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(&Event{EventType: Header, Meta: map[string]any{"mode": "record"}})
	w.Close()
	if _, err := Create(path, true); err == nil {
		t.Fatal("exclusive create overwrote an existing cassette")
	}
	evs, err := ReadFile(path)
	if err != nil || len(evs) != 1 {
		t.Fatalf("read back: %v %v", evs, err)
	}
}

func TestReadRejectsOtherSchemaVersion(t *testing.T) {
	_, err := Read(bytes.NewBufferString(`{"schema_version":99,"event_type":"header"}` + "\n"))
	if err == nil {
		t.Fatal("accepted unknown schema version")
	}
}
