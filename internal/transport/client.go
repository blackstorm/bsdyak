package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
)

// Client communicates with remote yakd exclusively over a yamux-multiplexed tunnel
// (either SSH stdin/stdout running `yak bridge` or an in-memory pipe for testing).
type Client struct {
	session    *yamux.Session
	httpClient *http.Client
	cleanup    func() error
}

// DialSSH launches system `ssh -T <host> yak bridge` (inheriting ~/.ssh/config,
// ProxyJump, custom ports, ssh-agent, and hardware keys) and establishes a yamux session.
func DialSSH(ctx context.Context, targetHost string, sshArgs ...string) (*Client, error) {
	host, extraArgs := parseSSHHost(targetHost)
	args := []string{"-T"}
	if cfgPath := os.Getenv("YAK_SSH_CONFIG"); cfgPath != "" {
		args = append(args, "-F", cfgPath)
	}
	args = append(args, extraArgs...)
	args = append(args, sshArgs...)
	args = append(args, host, "yak", "bridge")

	cmd := exec.CommandContext(ctx, "ssh", args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ssh stdout pipe: %w", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ssh %s: %w", host, err)
	}

	rwc := &stdioConn{r: stdout, w: stdin}
	client, err := NewClientFromConn(rwc, func() error {
		_ = stdin.Close()
		waitErr := cmd.Wait()
		if waitErr != nil && stderr.Len() > 0 {
			return fmt.Errorf("ssh exited (%w): %s", waitErr, strings.TrimSpace(stderr.String()))
		}
		return waitErr
	})
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, fmt.Errorf("ssh bridge handshake failed (%s): %w", strings.TrimSpace(stderr.String()), err)
	}
	return client, nil
}

// parseSSHHost allows host strings like "user@host:2222" alongside standard SSH aliases.
func parseSSHHost(target string) (string, []string) {
	if strings.Count(target, ":") == 1 && !strings.Contains(target, "[") {
		idx := strings.LastIndex(target, ":")
		port := target[idx+1:]
		isDigits := len(port) > 0
		for i := 0; i < len(port); i++ {
			if port[i] < '0' || port[i] > '9' {
				isDigits = false
				break
			}
		}
		if isDigits {
			return target[:idx], []string{"-p", port}
		}
	}
	return target, nil
}

// NewClientFromConn creates a Client over any bidirectional byte stream (SSH or net.Pipe).
func NewClientFromConn(rwc io.ReadWriteCloser, cleanup func() error) (*Client, error) {
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	cfg.KeepAliveInterval = 15 * time.Second

	session, err := yamux.Client(rwc, cfg)
	if err != nil {
		return nil, fmt.Errorf("create yamux client: %w", err)
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return session.Open()
			},
		},
	}

	return &Client{
		session:    session,
		httpClient: httpClient,
		cleanup:    cleanup,
	}, nil
}

func (c *Client) Close() error {
	var err error
	if c.session != nil {
		err = c.session.Close()
	}
	if c.cleanup != nil {
		if cErr := c.cleanup(); err == nil {
			err = cErr
		}
	}
	return err
}

func (c *Client) DoJSON(ctx context.Context, method, path string, reqBody any, respOut any) error {
	var bodyReader io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, "http://yakd"+path, bodyReader)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		return fmt.Errorf("yakd HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if respOut != nil {
		return json.NewDecoder(resp.Body).Decode(respOut)
	}
	return nil
}

func (c *Client) PostStream(ctx context.Context, path string, contentType string, body io.Reader, headers map[string]string, respOut any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://yakd"+path, body)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		return fmt.Errorf("yakd HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if respOut != nil {
		return json.NewDecoder(resp.Body).Decode(respOut)
	}
	return nil
}

// DoStream performs a request expecting a streaming response (NDJSON deploy events or logs).
// The caller must close the returned io.ReadCloser.
func (c *Client) DoStream(ctx context.Context, method, path string, reqBody any) (io.ReadCloser, error) {
	var bodyReader io.Reader
	if reqBody != nil {
		raw, err := json.Marshal(reqBody)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://yakd"+path, bodyReader)
	if err != nil {
		return nil, err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("yakd HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}

// OpenExecStream opens an HTTP-upgraded raw duplex yamux stream to /exec for interactive or piped execution.
func (c *Client) OpenExecStream(ctx context.Context, path string, payload any) (net.Conn, *bufio.Reader, error) {
	conn, err := c.session.Open()
	if err != nil {
		return nil, nil, err
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://yakd"+path, bytes.NewReader(raw))
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "yak-exec")

	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, nil, fmt.Errorf("exec upgrade rejected (%d): %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return conn, br, nil
}
