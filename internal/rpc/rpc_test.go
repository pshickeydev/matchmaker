package rpc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// echoHandler records requests and answers with a fixed message.
type echoHandler struct {
	got chan Request
}

func (h *echoHandler) Handle(_ context.Context, req Request) (Response, error) {
	h.got <- req
	if req.Method == "boom" {
		return Response{}, errors.New("exploded")
	}
	return Response{Message: "ok:" + string(req.Method)}, nil
}

func TestServerClientRoundTrip(t *testing.T) {
	stateDir := t.TempDir()
	handler := &echoHandler{got: make(chan Request, 4)}
	server := NewServer(stateDir, handler)
	if err := server.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { server.Close() })

	client, err := Connect(stateDir)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()
	response, err := client.Call(context.Background(), Request{
		IdempotencyKey: "k1", Method: MethodGoalStatus, GoalID: "g1",
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if response.Message != "ok:goal_status" {
		t.Errorf("response = %+v", response)
	}
	select {
	case req := <-handler.got:
		if req.GoalID != "g1" || req.IdempotencyKey != "k1" {
			t.Errorf("handler saw %+v", req)
		}
	default:
		t.Fatal("handler never saw the request")
	}
}

func TestHandlerErrorsSurfaceAsMessage(t *testing.T) {
	stateDir := t.TempDir()
	server := NewServer(stateDir, &echoHandler{got: make(chan Request, 4)})
	if err := server.Listen(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	client, err := Connect(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	response, err := client.Call(context.Background(), Request{Method: "boom"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if response.Message != "exploded" {
		t.Errorf("response = %+v", response)
	}
}

func TestStaleSocketRemovedUnderLock(t *testing.T) {
	stateDir := t.TempDir()
	sockPath := filepath.Join(stateDir, "rpc", sockName)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sockPath, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := NewServer(stateDir, &echoHandler{got: make(chan Request, 1)})
	// The daemon holds the exclusive lock when NewServer runs, so the
	// leftover file is stale and removed (DESIGN §3).
	if err := server.Listen(); err != nil {
		t.Fatalf("Listen with stale socket: %v", err)
	}
	t.Cleanup(func() { server.Close() })
	if _, err := Connect(stateDir); err != nil {
		t.Fatalf("Connect after stale cleanup: %v", err)
	}
}

func TestConnectWithoutDaemon(t *testing.T) {
	if _, err := Connect(t.TempDir()); err == nil {
		t.Fatal("connect succeeded without a daemon")
	}
}
