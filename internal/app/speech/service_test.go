package speech

import "testing"

func TestRealtimeURLDerivesWorkspaceWebSocketEndpoint(t *testing.T) {
	got, err := RealtimeURL("https://ws_1.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1", DefaultModel)
	if err != nil {
		t.Fatalf("RealtimeURL: %v", err)
	}
	want := "wss://ws_1.ap-southeast-1.maas.aliyuncs.com/api-ws/v1/realtime?model=qwen-asr-realtime"
	if got != want {
		t.Fatalf("RealtimeURL = %q, want %q", got, want)
	}
}
