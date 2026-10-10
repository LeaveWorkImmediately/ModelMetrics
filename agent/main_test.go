package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInventoryDecoding(t *testing.T) {
	for _, raw := range []string{`null`, `[{"physicalId":"0","name":"chip","unknown":true}]`, `[{"physicalId":"0","name":"chip"},{"physicalId":"0","name":"chip"}]`} {
		if _, err := decodeInventory(strings.NewReader(raw)); err == nil {
			t.Fatal("accepted invalid driver inventory")
		}
	}
	devices, err := decodeInventory(strings.NewReader(`[{"physicalId":"100","name":"chip","model":"unknown","vendor":"Ascend"}]`))
	if err != nil || len(devices) != 1 || devices[0].PhysicalID != "100" {
		t.Fatal("physical inventory decode failed")
	}
}

func TestRetryPreservesSequence(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("machine"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer machine" {
			t.Error("missing machine auth")
		}
		if r.Method == "POST" {
			_, _ = w.Write([]byte(`{"sessionId":"session"}`))
			return
		}
		if r.URL.Path != "/internal/v1/agent-sessions/session/snapshots/1" {
			t.Error("retry sequence changed")
		}
		calls++
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	a := agent{client: server.Client(), base: server.URL, tokenFile: token, node: "node", epoch: "epoch"}
	snapshot := inventory{ObservedAt: time.Now()}
	if a.send(context.Background(), snapshot) == nil {
		t.Fatal("expected temporary delivery failure")
	}
	if err := a.send(context.Background(), snapshot); err != nil || a.sequence != 2 {
		t.Fatalf("retry: %v", err)
	}
}
