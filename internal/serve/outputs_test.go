package serve

import (
	"fmt"
	"strings"
	"testing"

	"github.com/m314dev/m314dl/internal/manifest"
)

func TestBuildOutputsLogsAudioTrack(t *testing.T) {
	var logs []string
	logv := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	streams := []*manifest.Stream{
		{ID: "v", Type: manifest.Video, Codecs: "avc1.4d402a", Init: &manifest.InitMap{}},
		{ID: "a0", Type: manifest.Audio, Codecs: "ec-3", Language: "en-US", Init: &manifest.InitMap{}},
	}
	_, _, _, jobs, err := BuildOutputs(Options{Format: "dash"}, streams, t.TempDir(), logv)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("jobs = %d, want video and audio", len(jobs))
	}
	if output := strings.Join(logs, "\n"); !strings.Contains(output, "a0") || !strings.Contains(output, "ec-3") || !strings.Contains(output, "en-US") {
		t.Fatalf("selected audio missing from logs: %s", output)
	}
}
