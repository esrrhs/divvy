package agent

import "testing"

func TestNormalizeNetCheckCommand(t *testing.T) {
	got := normalizeNetCheckCommand("nc -z 127.0.0.1 8080")
	want := "bash -c 'exec 3<>/dev/tcp/127.0.0.1/8080'"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	cmds := normalizeDoDCommands([]string{"nc -z localhost 90", "go test ./..."})
	if cmds[0] != "bash -c 'exec 3<>/dev/tcp/localhost/90'" {
		t.Fatalf("normalized[0]=%q", cmds[0])
	}
	if cmds[1] != "go test ./..." {
		t.Fatalf("other cmds must pass through: %q", cmds[1])
	}
	if got := normalizeNetCheckCommand("curl -sf http://127.0.0.1:8080/"); got != "curl -sf http://127.0.0.1:8080/" {
		t.Fatalf("curl must be untouched: %q", got)
	}
}
