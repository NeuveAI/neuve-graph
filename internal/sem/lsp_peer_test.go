package sem

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func lspTestLimits() LSPLimits {
	return LSPLimits{FrameBytes: 64 << 10, TotalBytes: 1 << 20, StderrBytes: 16 << 10, Requests: 16, RequestTimeout: 400 * time.Millisecond, ShutdownGrace: 200 * time.Millisecond}
}
func lspTestFrame(value any) []byte {
	body, _ := json.Marshal(value)
	return []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body))
}
func TestLSPFrameAdmission(t *testing.T) {
	for _, raw := range []string{"Content-Length: -1\r\n\r\n", "Content-Length: 2\r\nContent-Length: 2\r\n\r\n{}", "Content-Length: 99999\r\n\r\n", "Content-Length: 3\r\n\r\n{}", "Content-Length: 1\r\n\r\n\xff", "Content-Length: 1\r\n\r\nx", strings.Repeat("x", 9000)} {
		if _, err := readLSPFrame(bufio.NewReaderSize(strings.NewReader(raw), 8192), 100); err == nil {
			t.Fatalf("admitted invalid frame %q", raw[:min(len(raw), 40)])
		}
	}
	raw := append(lspTestFrame(map[string]string{"text": "😀"}), lspTestFrame(map[string]int{"id": 2})...)
	reader := bufio.NewReaderSize(&lspFragmentReader{data: raw}, 8192)
	for i := 0; i < 2; i++ {
		if _, err := readLSPFrame(reader, 100); err != nil {
			t.Fatal(err)
		}
	}
}

type lspFragmentReader struct{ data []byte }

func (r *lspFragmentReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

// Only native tests invoke this explicitly selected helper; production admits clangd.
func TestLSPPeerHelper(t *testing.T) {
	mode := os.Getenv("NEUVE_LSP_TEST_HELPER")
	if mode == "" {
		return
	}
	if mode == "policy-leaf" {
		fmt.Print("same-executable-child-ran")
		os.Exit(0)
	}
	if mode == "policy" {
		connection, err := net.DialTimeout("tcp", os.Getenv("NEUVE_LSP_TEST_ADDRESS"), 200*time.Millisecond)
		if err == nil {
			connection.Close()
			fmt.Println("network-escaped")
			os.Exit(2)
		}
		if exec.Command("/usr/bin/true").Run() == nil {
			fmt.Println("child-escaped")
			os.Exit(3)
		}
		if _, err := os.ReadFile(os.Getenv("NEUVE_LSP_TEST_FOREIGN_SOURCE")); err == nil {
			fmt.Println("foreign-source-escaped")
			os.Exit(6)
		}
		executable, err := os.Executable()
		if err != nil {
			os.Exit(7)
		}
		child := exec.Command(executable, "-test.run=^TestLSPPeerHelper$")
		child.Env = append(os.Environ(), "NEUVE_LSP_TEST_HELPER=policy-leaf")
		if body, err := child.CombinedOutput(); err == nil || bytes.Contains(body, []byte("same-executable-child-ran")) {
			fmt.Println("same-executable-child-escaped")
			os.Exit(8)
		}
		fmt.Println("network-and-child-denied")
		os.Exit(0)
	}
	if mode == "no-read" {
		select {}
	}
	if mode == "stderr" {
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("x"), 100<<10))
		select {}
	}
	reader := bufio.NewReaderSize(os.Stdin, 8192)
	for {
		body, err := readLSPFrame(reader, 1<<20)
		if err != nil {
			os.Exit(0)
		}
		var message lspMessage
		if json.Unmarshal(body, &message) != nil {
			os.Exit(4)
		}
		if message.Method == "exit" {
			os.Exit(0)
		}
		if len(message.ID) == 0 {
			continue
		}
		if mode == "hang" || mode == "shutdown-hang" && message.Method == "shutdown" {
			select {}
		}
		if mode == "late" {
			time.Sleep(100 * time.Millisecond)
		}
		if mode == "eof" {
			fmt.Print("Content-Length: 100\r\n\r\n{")
			os.Exit(0)
		}
		if mode == "foreign" {
			fmt.Printf("%s", lspTestFrame(map[string]any{"jsonrpc": "2.0", "id": 999, "result": nil}))
			continue
		}
		if mode == "server-request" {
			fmt.Printf("%s", lspTestFrame(map[string]any{"jsonrpc": "2.0", "id": "server-1", "method": "workspace/applyEdit", "params": map[string]any{}}))
			answer, err := readLSPFrame(reader, 1000)
			if err != nil || !bytes.Contains(answer, []byte("-32601")) {
				os.Exit(5)
			}
		}
		if mode == "malformed" {
			fmt.Print("Content-Length: 1\r\n\r\nx")
			continue
		}
		reply := lspTestFrame(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": map[string]any{"literal": "😀"}})
		fmt.Printf("%s", reply)
		if mode == "duplicate" {
			fmt.Printf("%s", reply)
		}
	}
}
func lspTestPeer(t *testing.T, mode string) *lspPeer {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestLSPPeerHelper$")
	cmd.Env = append(os.Environ(), "NEUVE_LSP_TEST_HELPER="+mode)
	peer, err := startLSPPeer(cmd, lspTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.close() })
	return peer
}
func TestLSPPeerFiniteFailuresAndCleanup(t *testing.T) {
	for _, mode := range []string{"foreign", "eof", "malformed", "stderr", "hang"} {
		t.Run(mode, func(t *testing.T) {
			peer := lspTestPeer(t, mode)
			if _, err := peer.request(context.Background(), "fixture", nil); err == nil {
				t.Fatal("failed peer created success")
			}
			_ = peer.close()
			select {
			case <-peer.exited:
			case <-time.After(time.Second):
				t.Fatal("peer not reaped")
			}
		})
	}
	for _, mode := range []string{"normal", "server-request"} {
		t.Run(mode, func(t *testing.T) {
			peer := lspTestPeer(t, mode)
			raw, err := peer.request(context.Background(), "fixture", nil)
			if err != nil || !bytes.Contains(raw, []byte("😀")) {
				t.Fatalf("literal response %s: %v", raw, err)
			}
		})
	}
}
func TestLSPPeerAbortPendingAndShutdownHang(t *testing.T) {
	peer := lspTestPeer(t, "hang")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, err := peer.request(ctx, "fixture", nil); err == nil {
		t.Fatal("cancel created success")
	}
	_ = peer.close()
	select {
	case <-peer.exited:
	default:
		t.Fatal("cancelled peer not reaped")
	}
	hanging := lspTestPeer(t, "shutdown-hang")
	if _, err := hanging.request(context.Background(), "fixture", nil); err != nil {
		t.Fatal(err)
	}
	_ = hanging.close()
	select {
	case <-hanging.exited:
	default:
		t.Fatal("shutdown peer not reaped")
	}
}

func TestLSPPeerDuplicateResponseCannotSealLifecycle(t *testing.T) {
	peer := lspTestPeer(t, "duplicate")
	_, err := peer.request(context.Background(), "fixture", nil)
	if err == nil && peer.close() == nil {
		t.Fatal("duplicate response sealed as clean peer lifecycle")
	}
}

func TestLSPStalledWriterAndLateResponse(t *testing.T) {
	for _, mode := range []string{"no-read", "late"} {
		t.Run(mode, func(t *testing.T) {
			peer := lspTestPeer(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			var params any
			if mode == "no-read" {
				params = strings.Repeat("x", 60<<10)
			}
			if _, err := peer.request(ctx, "fixture", params); err == nil {
				t.Fatal("stalled/cancelled operation produced success")
			}
			_ = peer.close()
			select {
			case <-peer.exited:
			default:
				t.Fatal("interrupted peer not reaped")
			}
		})
	}
}

func TestLSPNotificationWriteHasConfiguredDeadlineAndReaps(t *testing.T) {
	peer := lspTestPeer(t, "no-read")
	peer.limits.FrameBytes = 8 << 20
	peer.limits.TotalBytes = 16 << 20
	done := make(chan error, 1)
	go func() { done <- peer.notify(context.Background(), "textDocument/didOpen", strings.Repeat("x", 4<<20)) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("unread notification succeeded")
		}
	case <-time.After(peer.limits.RequestTimeout + peer.limits.ShutdownGrace + 250*time.Millisecond):
		_ = peer.cmd.Process.Kill()
		t.Fatal("notification exceeded finite deadline")
	}
	select {
	case <-peer.exited:
	default:
		t.Fatal("notification expiry returned before peer reap")
	}
}
