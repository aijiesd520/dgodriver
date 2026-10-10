package cdp

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestProtocolSnapshotAndOptionalValues(t *testing.T) {
	raw, err := os.ReadFile("protocol.json")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Domains []struct {
			Commands []json.RawMessage
			Types    []struct{ Event string }
		}
	}
	if err = json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	commands, events := 0, 0
	for _, d := range snapshot.Domains {
		commands += len(d.Commands)
		for _, typ := range d.Types {
			if typ.Event != "" {
				events++
			}
		}
	}
	if len(snapshot.Domains) != 56 || commands != 664 || events != 237 {
		t.Fatalf("snapshot mismatch: %d %d %d", len(snapshot.Domains), commands, events)
	}
	raw, err = json.Marshal(PageReloadParams{IgnoreCache: Ptr(false)})
	if err != nil || string(raw) != `{"ignoreCache":false}` {
		t.Fatal(string(raw), err)
	}
	raw, err = json.Marshal(FetchEnableParams{Patterns: []FetchRequestPattern{}})
	if err != nil || !strings.Contains(string(raw), `"patterns":[]`) {
		t.Fatal("explicit empty array omitted", string(raw), err)
	}
	raw, err = json.Marshal(RuntimeCallArgument{Value: json.RawMessage("null")})
	if err != nil || string(raw) != `{"value":null}` {
		t.Fatal(string(raw), err)
	}
	raw, err = json.Marshal(RuntimeCallArgument{})
	if err != nil || string(raw) != "{}" {
		t.Fatal(string(raw), err)
	}
}
