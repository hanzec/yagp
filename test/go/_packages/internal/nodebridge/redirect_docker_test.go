package nodebridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestDockerBridgeDiscoveryStopsOnCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var state redirectState
	if err := state.detectDocker(ctx, true, dockerSocketPath); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discovery returned %v", err)
	}
}

func TestLocalDockerBridgeNamesDistinguishesBadAndEmptyResponses(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "yagp-docker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	for _, test := range []struct {
		name string
		code int
		body string
	}{
		{"redirect", http.StatusFound, ""},
		{"unavailable", http.StatusServiceUnavailable, ""},
		{"invalid-json", http.StatusOK, "invalid JSON"},
		{"invalid-id", http.StatusOK, `[{"Name":"custom","Id":"invalidID0001234","Driver":"bridge"}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			socket := filepath.Join(socketDir, test.name)
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/networks" {
					t.Errorf("followed Docker API redirect to %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if test.code == http.StatusFound {
					w.Header().Set("Location", "/other-endpoint")
				}
				w.WriteHeader(test.code)
				_, _ = io.WriteString(w, test.body)
			})}
			t.Cleanup(func() { _ = server.Close() })
			go func() { _ = server.Serve(listener) }()
			got := localDockerBridgeNames(t.Context(), socket)
			if test.name == "invalid-id" {
				if got == nil || len(got) != 0 {
					t.Fatalf("valid API response without usable networks returned %v", got)
				}
			} else if got != nil {
				t.Fatalf("bad Docker response returned %v, want nil snapshot", got)
			}
		})
	}
}

func TestLocalDockerBridgeNamesUsesDockerNetworkOptions(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	socketDir, err := os.MkdirTemp("", "yagp-docker-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socket := filepath.Join(socketDir, "engine.sock")
	if got := localDockerBridgeNames(t.Context(), socket); got != nil {
		t.Fatalf("missing Docker socket returned %v", got)
	}

	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/networks" {
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if filters := r.URL.Query().Get("filters"); filters != `{"driver":["bridge"]}` {
			t.Errorf("Docker network filters = %s, want bridge driver", filters)
		}
		_, _ = io.WriteString(w, `[
			{"Name":"bridge","Driver":"bridge"},
			{"Name":"project_default","Id":"4d7902c53a231234","Driver":"bridge"},
			{"Name":"project_custom","Driver":"bridge","Options":{"com.docker.network.bridge.name":"compose-net"}},
			{"Name":"wrong-driver","Driver":"macvlan","Options":{"com.docker.network.bridge.name":"br-lan"}}
		]`)
	})}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()

	names := localDockerBridgeNames(t.Context(), socket)
	if len(names) != 3 {
		t.Fatalf("unexpected Docker bridge names %v", names)
	}
	for _, name := range []string{"docker0", "br-4d7902c53a23", "compose-net"} {
		if _, ok := names[name]; !ok {
			t.Fatalf("missing %s in Docker bridge names %v", name, names)
		}
	}
}

func TestWatchDockerNetworkEventsRefreshesAndReconnects(t *testing.T) {
	dir, err := os.MkdirTemp("", "yagp-docker-events-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "engine.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int32
	streams := make(chan chan string)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/events" {
			t.Errorf("unexpected Docker request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var filters map[string]json.RawMessage
		if err := json.Unmarshal([]byte(r.URL.Query().Get("filters")), &filters); err != nil {
			t.Errorf("decode Docker event filters: %v", err)
		}
		switch string(filters["type"]) {
		case `["network"]`, `{"network":true}`:
		default:
			t.Errorf("expected Docker network event filter, got %s", filters["type"])
		}
		value := r.URL.Query().Get("since")
		if since, err := strconv.ParseFloat(value, 64); err != nil || since <= 0 {
			t.Errorf("invalid Docker event replay start %q", value)
		}
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		flusher := w.(http.Flusher)
		flusher.Flush()
		events := make(chan string)
		select {
		case streams <- events:
		case <-r.Context().Done():
			return
		}
		for {
			select {
			case event, open := <-events:
				if !open {
					return
				}
				if _, err := io.WriteString(w, event+"\n"); err != nil {
					return
				}
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	const retryDelay = 10 * time.Millisecond
	refreshes := make(chan struct{}, 8)
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchDockerNetworkEvents(ctx, socket, retryDelay, func() {
			select {
			case refreshes <- struct{}{}:
			case <-ctx.Done():
			}
		})
	}()
	nextStream := func() chan string {
		t.Helper()
		select {
		case stream := <-streams:
			return stream
		case <-ctx.Done():
			t.Fatal("waiting for Docker event stream:", ctx.Err())
			return nil
		}
	}
	expectRefresh := func(after string) {
		t.Helper()
		select {
		case <-refreshes:
		case <-ctx.Done():
			t.Fatalf("missing Docker network refresh after %s: %v", after, ctx.Err())
		}
	}
	expectIdle := func(after string) {
		t.Helper()
		select {
		case <-refreshes:
			t.Fatalf("unexpected Docker network refresh after %s", after)
		case <-time.After(5 * retryDelay):
		}
	}
	send := func(stream chan string, event string) {
		t.Helper()
		select {
		case stream <- event:
		case <-ctx.Done():
			t.Fatalf("sending Docker event %s: %v", event, ctx.Err())
		}
	}

	stream := nextStream()
	expectRefresh("connection retry")
	expectIdle("initial refresh")
	send(stream, `{"Type":"container","Action":"start"}`)
	expectIdle("container start event")
	send(stream, `{"Type":"network","Action":"create","Actor":{"Attributes":{"type":"overlay"}}}`)
	expectIdle("overlay network create event")
	send(stream, `{"Type":"network","Action":"create","Actor":{"ID":"old","Attributes":{"type":"bridge"}}}`)
	expectRefresh("network create event")
	send(stream, `{"Type":"network","Action":"destroy","Actor":{"ID":"old","Attributes":{"type":"bridge"}}}`)
	expectRefresh("network destroy event")
	close(stream)
	nextStream()
	expectRefresh("reconnection")
	expectIdle("reconnection refresh")
	if got := requests.Load(); got != 3 {
		t.Fatalf("Docker event stream opened %d times, want 3", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Docker event watcher did not stop when canceled")
	}
}
