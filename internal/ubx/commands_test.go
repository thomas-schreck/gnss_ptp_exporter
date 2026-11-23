package ubx

import (
	"encoding/json"
	"strings"
	"testing"
)

// A device path with quotes, backslashes or a newline must stay inside its
// JSON string, so gpsd sees exactly three well-formed commands.
func TestPollCommandsEscapeDevicePath(t *testing.T) {
	device := "/dev/tty\"evil\\\n?WATCH={\"enable\":false}"

	cmds, err := pollCommands(device)
	if err != nil {
		t.Fatalf("pollCommands: %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(cmds, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d command lines, want 3: %q", len(lines), cmds)
	}

	for i, line := range lines {
		verb, body, ok := strings.Cut(line, "=")
		if !ok || (verb != "?WATCH" && verb != "?DEVICE") {
			t.Fatalf("line %d has unexpected verb: %q", i, line)
		}

		var got map[string]any
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i, err)
		}

		key := "path"
		if verb == "?WATCH" {
			key = "device"
		}

		if got[key] != device {
			t.Fatalf("line %d: %s = %q, want %q", i, key, got[key], device)
		}
	}
}
