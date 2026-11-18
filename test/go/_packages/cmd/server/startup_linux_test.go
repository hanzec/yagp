package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"yagp/pkg/nodegrpc"
)

func TestServerStartupAndSignalShutdown(t *testing.T) {
	if path := os.Getenv("YAGP_TEST_SERVER_CONFIG"); path != "" {
		flag.CommandLine = flag.NewFlagSet("yagp", flag.ExitOnError)
		os.Args = []string{"yagp", "-c", path}
		main()
		return
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	reports, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer reports.Close()
	address := listener.Addr().String()
	reportAddress := reports.Addr().String()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	config := fmt.Sprintf("host: 127.0.0.1\nserver:\n  port: %q\n  mode: release\ndatabase:\n  type: sqlite\n  path: %q\nreport:\n  listen: %q\nlog:\n  level: info\n  output: stdout\n", address, filepath.Join(dir, "panel.db"), reportAddress)
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	args := []string{"-test.run=^TestServerStartupAndSignalShutdown$"}
	// A test subprocess has the same instrumented binary as its parent. Share
	// the Go test scratch directory so its actual startup coverage is collected.
	if coverage := flag.Lookup("test.gocoverdir"); coverage != nil && coverage.Value.String() != "" {
		args = append(args, "-test.gocoverdir="+coverage.Value.String())
	}
	child := exec.CommandContext(ctx, os.Args[0], args...)
	child.Dir = dir
	child.Env = append(os.Environ(), "YAGP_TEST_SERVER_CONFIG="+path, "YAGP_HOST=127.0.0.1", "YAGP_DATABASE_TYPE=sqlite", "YAGP_DATABASE_DSN=")
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	_ = listener.Close()
	_ = reports.Close()
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = child.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		<-done
	})

	httpClient := &http.Client{Timeout: 100 * time.Millisecond}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/api/v1/health", nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := httpClient.Do(req)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-done:
			t.Fatalf("server exited before becoming ready: %v\n%s", waitErr, &output)
		case <-ctx.Done():
			t.Fatal("server did not become ready before the deadline")
		case <-time.After(25 * time.Millisecond):
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+reportAddress+nodegrpc.SessionPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("node session without credentials = HTTP %d", res.StatusCode)
	}
	// A 100 Continue proves the handler is waiting for this request body.
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "POST /api/v1/system/email/test HTTP/1.1\r\nHost: %s\r\nOrigin: http://%s\r\nX-Authentik-Username: admin\r\nX-Authentik-Groups: yagp-admins\r\nContent-Type: application/json\r\nExpect: 100-continue\r\nContent-Length: 1\r\n\r\n", address, address); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusContinue {
		t.Fatalf("request did not reach the handler: %s", response.Status)
	}
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	for {
		probe, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err != nil {
			break
		}
		probe.Close()
		if ctx.Err() != nil {
			t.Fatal("server still accepts connections after SIGTERM")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Finish the body after the listener closes. The request must still get its HTTP response.
	if _, err := conn.Write([]byte("!")); err != nil {
		t.Fatal(err)
	}
	response, err = http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("in-flight request was interrupted: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed JSON during shutdown: %s", response.Status)
	}
	<-done
	if waitErr != nil || !bytes.Contains(output.Bytes(), []byte("yagp 正在关闭")) {
		t.Fatalf("graceful shutdown failed: %v\n%s", waitErr, &output)
	}
}
