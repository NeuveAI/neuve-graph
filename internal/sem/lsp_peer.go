package sem

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type LSPErrorReason string

type LSPEnrichmentError struct {
	Reason    LSPErrorReason
	Detail    string
	Cause     error
	Requested *LSPRequestedSelection
	Raw       json.RawMessage
}

func (e *LSPEnrichmentError) Error() string { return "local LSP " + string(e.Reason) + ": " + e.Detail }
func (e *LSPEnrichmentError) Unwrap() error { return e.Cause }
func lspError(reason LSPErrorReason, detail string, cause error) error {
	return &LSPEnrichmentError{Reason: reason, Detail: detail, Cause: cause}
}

type LSPLimits struct {
	FrameBytes, TotalBytes, StderrBytes, Requests int
	RequestTimeout, ShutdownGrace                 time.Duration
}

func (l LSPLimits) valid() bool {
	return l.FrameBytes > 0 && l.FrameBytes <= 8<<20 && l.TotalBytes >= l.FrameBytes && l.TotalBytes <= 64<<20 && l.StderrBytes > 0 && l.StderrBytes <= 1<<20 && l.Requests > 0 && l.Requests <= 128 && l.RequestTimeout > 0 && l.RequestTimeout <= time.Minute && l.ShutdownGrace > 0 && l.ShutdownGrace <= 5*time.Second
}

type lspBoundedReader struct {
	reader    io.Reader
	remaining int
}

func (r *lspBoundedReader) Read(buffer []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, lspError("bounds", "stdout exceeded lifetime bound", nil)
	}
	if len(buffer) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	n, err := r.reader.Read(buffer)
	r.remaining -= n
	return n, err
}

type lspMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type lspPeer struct {
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	messages      chan lspMessage
	fatal         chan error
	exited        chan struct{}
	exitErr       error
	limits        LSPLimits
	writes        sync.Mutex
	next          int
	received      int
	notifications []lspMessage
}

func readLSPFrame(reader *bufio.Reader, limit int) ([]byte, error) {
	length := -1
	headerBytes := 0
	for {
		fragment, err := reader.ReadSlice('\n')
		line := string(fragment)
		headerBytes += len(line)
		if headerBytes > 8192 {
			return nil, lspError("frame", "header exceeded bound", nil)
		}
		if err != nil {
			return nil, err
		}
		if !strings.HasSuffix(line, "\r\n") {
			return nil, lspError("frame", "header is not CRLF framed", nil)
		}
		line = strings.TrimSuffix(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, lspError("frame", "invalid header", nil)
		}
		if strings.EqualFold(name, "Content-Length") {
			value = strings.TrimSpace(value)
			if length != -1 || value == "" || strings.Trim(value, "0123456789") != "" {
				return nil, lspError("frame", "invalid Content-Length", nil)
			}
			n, err := strconv.Atoi(value)
			if err != nil || n > limit {
				return nil, lspError("frame", "body exceeded bound", err)
			}
			length = n
		} else if !strings.EqualFold(name, "Content-Type") {
			return nil, lspError("frame", "unsupported header", nil)
		}
	}
	if length < 0 {
		return nil, lspError("frame", "missing Content-Length", nil)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, err
	}
	if !utf8.Valid(body) || !json.Valid(body) {
		return nil, lspError("protocol", "invalid UTF-8 JSON", nil)
	}
	return body, nil
}

func startLSPPeer(cmd *exec.Cmd, limits LSPLimits) (*lspPeer, error) {
	if !limits.valid() {
		return nil, lspError("configuration", "invalid finite limits", nil)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	cmd.WaitDelay = limits.ShutdownGrace
	p := &lspPeer{cmd: cmd, stdin: stdin, messages: make(chan lspMessage, 16), fatal: make(chan error, 1), exited: make(chan struct{}), limits: limits}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return nil, lspError("peer-exit", "could not start peer", err)
	}
	fail := func(err error) {
		select {
		case p.fatal <- err:
		default:
		}
		_ = cmd.Process.Kill()
	}
	go func() {
		reader := bufio.NewReaderSize(&lspBoundedReader{reader: stdout, remaining: limits.TotalBytes}, 8192)
		total := 0
		for {
			body, err := readLSPFrame(reader, limits.FrameBytes)
			if err != nil {
				fail(lspError("frame", "peer stdout ended or invalid", err))
				return
			}
			total += len(body)
			if total > limits.TotalBytes {
				fail(lspError("bounds", "stdout exceeded lifetime bound", nil))
				return
			}
			var message lspMessage
			if err := json.Unmarshal(body, &message); err != nil || message.JSONRPC != "2.0" || (message.Method == "" && len(message.ID) == 0) {
				fail(lspError("protocol", "invalid message envelope", err))
				return
			}
			select {
			case p.messages <- message:
			default:
				fail(lspError("bounds", "peer message queue exceeded bound", nil))
				return
			}
		}
	}()
	go func() {
		count, err := io.Copy(io.Discard, io.LimitReader(stderr, int64(limits.StderrBytes)+1))
		if count > int64(limits.StderrBytes) {
			fail(lspError("bounds", "stderr exceeded lifetime bound", nil))
		} else if err != nil {
			fail(lspError("transport", "stderr read failed", err))
		}
	}()
	go func() { p.exitErr = cmd.Wait(); close(p.exited) }()
	return p, nil
}

func (p *lspPeer) write(ctx context.Context, value any) error {
	operation, cancel := context.WithTimeout(ctx, p.limits.RequestTimeout)
	defer cancel()
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(body) > p.limits.FrameBytes {
		return lspError("bounds", "outgoing frame exceeded bound", nil)
	}
	done := make(chan error, 1)
	go func() {
		p.writes.Lock()
		defer p.writes.Unlock()
		_, err := fmt.Fprintf(p.stdin, "Content-Length: %d\r\n\r\n%s", len(body), body)
		done <- err
	}()
	select {
	case err := <-done:
		if operation.Err() == nil {
			return err
		}
	case <-operation.Done():
	}
	_ = p.cmd.Process.Kill()
	select {
	case <-p.exited:
	case <-time.After(p.limits.ShutdownGrace):
		return lspError("cleanup", "write expiry could not reap peer within grace", operation.Err())
	}
	reason := LSPErrorReason("deadline")
	if ctx.Err() != nil {
		reason = "cancelled"
	}
	return lspError(reason, "write interrupted", operation.Err())
}
func (p *lspPeer) notify(ctx context.Context, method string, params any) error {
	return p.write(ctx, map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}
func (p *lspPeer) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	p.received++
	if p.received > p.limits.Requests {
		return nil, lspError("bounds", "finite request set exceeded bound", nil)
	}
	p.next++
	id := p.next
	operation, cancel := context.WithTimeout(ctx, p.limits.RequestTimeout)
	defer cancel()
	if err := p.write(operation, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		select {
		case <-operation.Done():
			// Cancellation is advisory; force cleanup prevents any late success admission.
			grace, stop := context.WithTimeout(context.Background(), p.limits.ShutdownGrace)
			_ = p.notify(grace, "$/cancelRequest", map[string]int{"id": id})
			stop()
			_ = p.cmd.Process.Kill()
			reason := LSPErrorReason("deadline")
			if ctx.Err() != nil {
				reason = "cancelled"
			}
			return nil, lspError(reason, "request did not complete", operation.Err())
		case err := <-p.fatal:
			return nil, err
		case <-p.exited:
			return nil, lspError("peer-exit", "peer exited before response", p.exitErr)
		case message := <-p.messages:
			if operation.Err() != nil {
				return nil, lspError("cancelled", "late response refused", operation.Err())
			}
			if message.Method != "" {
				if len(message.ID) > 0 {
					if err := p.write(operation, map[string]any{"jsonrpc": "2.0", "id": message.ID, "error": map[string]any{"code": -32601, "message": "Method not supported by finite client"}}); err != nil {
						return nil, err
					}
				} else {
					p.notifications = append(p.notifications, message)
				}
				continue
			}
			var received int
			if err := json.Unmarshal(message.ID, &received); err != nil || received != id {
				return nil, lspError("response-identity", "foreign or duplicate response", err)
			}
			if (message.Error == nil) == (len(message.Result) == 0) {
				return nil, lspError("protocol", "response must have exactly one result or error", nil)
			}
			if message.Error != nil {
				raw, _ := json.Marshal(message.Error)
				return nil, &LSPEnrichmentError{Reason: "feature-error", Detail: message.Error.Message, Raw: raw}
			}
			return message.Result, nil
		}
	}
}
func (p *lspPeer) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), p.limits.ShutdownGrace)
	defer cancel()
	_, shutdownErr := p.request(ctx, "shutdown", nil)
	_ = p.notify(ctx, "exit", nil)
	_ = p.stdin.Close()
	select {
	case <-p.exited:
		return errors.Join(shutdownErr, p.exitErr)
	case <-ctx.Done():
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.exited:
		return errors.Join(shutdownErr, p.exitErr)
	case <-time.After(p.limits.ShutdownGrace):
		return errors.New("local LSP peer reap deadline exceeded")
	}
}
