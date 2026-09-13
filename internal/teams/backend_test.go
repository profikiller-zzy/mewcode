package teams

import "testing"

func TestDetectBackendAlwaysUsesInProcess(t *testing.T) {
	t.Setenv("TMUX", "/tmp/sock,1,0")
	t.Setenv("ITERM_SESSION_ID", "w0t0p0:ABC")
	if got := DetectBackend(); got != ModeInProcess {
		t.Fatalf("DetectBackend() = %q, want %q", got, ModeInProcess)
	}
}
