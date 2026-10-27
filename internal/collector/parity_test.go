package collector

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"liveprobe/internal/core"
)

// Adapter differential, Go side: runs tools/parity/adapter-parity.mts's corpus through the Go
// adapters and writes adapter-go.json. Skipped unless LIVEPROBE_PARITY_DIR is set.
func TestAdapterParityWithTypeScript(t *testing.T) {
	dir := os.Getenv("LIVEPROBE_PARITY_DIR")
	if dir == "" {
		t.Skip("LIVEPROBE_PARITY_DIR not set")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "adapter-corpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	var corpus []any
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	out := map[string][]any{}
	for name, adapt := range adapters {
		results := make([]any, len(corpus))
		for i, item := range corpus {
			results[i] = run(adapt, item)
		}
		out[name] = results
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "adapter-go.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(adapt Adapter, item any) (res any) {
	defer func() {
		if p := recover(); p != nil {
			res = []any{map[string]string{"thrown": fmt.Sprint(p)}}
		}
	}()
	events := adapt(item)
	if events == nil {
		events = []core.Event{}
	}
	return events
}
