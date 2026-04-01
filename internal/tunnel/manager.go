package tunnel

import (
	"bufio"
	"context"
	"log"
	"os/exec"
	"sync"
	"time"

	"lantern/internal/cf"
	"lantern/internal/config"
	"lantern/internal/store"
)

// TunnelStatus is the public status returned by Status().
type TunnelStatus struct {
	TunnelID  string    `json:"tunnel_id,omitempty"`
	Running   bool      `json:"running"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// Manager owns the cloudflared subprocess lifecycle.
type Manager struct {
	cf    *cf.Client
	store *store.Store
	cfg   *config.Config

	rootCtx context.Context // application lifetime context for subprocess lifecycle
	mu      sync.Mutex
	cmd     *exec.Cmd
	running bool
	cancel  context.CancelFunc // cancels the watchAndRestart goroutine
}

func New(cfg *config.Config, st *store.Store, cfClient *cf.Client) *Manager {
	return &Manager{cfg: cfg, store: st, cf: cfClient}
}

// StartIfConfigured auto-starts cloudflared if tunnel credentials are already
// persisted in the store (e.g. after a container restart).
func (m *Manager) StartIfConfigured(ctx context.Context) error {
	m.rootCtx = ctx
	info := m.store.GetTunnel()
	if info == nil {
		return nil
	}
	// Restore the tunnel ID into the CF client so ingress methods work.
	m.cf.SetTunnelID(info.TunnelID)
	log.Printf("tunnel: restoring tunnel %s from store", info.TunnelID)
	return m.startProcess(ctx, info.Token)
}

// Create calls the Cloudflare API to create a new tunnel named "lantern",
// persists credentials, and starts cloudflared.
func (m *Manager) Create(ctx context.Context) (*store.TunnelInfo, error) {
	tunnelID, token, err := m.cf.CreateTunnel(ctx, "lantern")
	if err != nil {
		return nil, err
	}
	info := &store.TunnelInfo{
		TunnelID:  tunnelID,
		Token:     token,
		CreatedAt: time.Now(),
	}
	m.store.SetTunnel(info)
	m.store.SaveLog("tunnel")
	if err := m.startProcess(m.rootCtx, token); err != nil {
		return nil, err
	}
	return info, nil
}

// Delete stops cloudflared and deletes the tunnel from Cloudflare.
func (m *Manager) Delete(ctx context.Context) error {
	info := m.store.GetTunnel()
	m.Stop()
	if info != nil {
		if err := m.cf.DeleteTunnel(ctx, info.TunnelID); err != nil {
			return err
		}
	}
	m.store.ClearTunnel()
	m.cf.SetTunnelID("")
	m.store.SaveLog("tunnel")
	return nil
}

// Status returns the current tunnel state.
func (m *Manager) Status() TunnelStatus {
	m.mu.Lock()
	running := m.running
	m.mu.Unlock()

	info := m.store.GetTunnel()
	if info == nil {
		return TunnelStatus{}
	}
	return TunnelStatus{
		TunnelID:  info.TunnelID,
		Running:   running,
		CreatedAt: info.CreatedAt,
	}
}

// startProcess launches cloudflared as a managed subprocess.
func (m *Manager) startProcess(ctx context.Context, token string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.running {
		return nil
	}

	watchCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	if err := m.launchCmd(watchCtx, token); err != nil {
		cancel()
		return err
	}

	go m.watchAndRestart(watchCtx, token)
	return nil
}

// launchCmd starts the cloudflared process. Must be called with m.mu held.
func (m *Manager) launchCmd(ctx context.Context, token string) error {
	cmd := exec.CommandContext(ctx, "/cloudflared", //nolint:gosec // binary path is hardcoded; token from secure store
		"tunnel", "--no-autoupdate", "run", "--token", token)

	// Pipe stdout and stderr to the logger with a prefix.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	pipe := func(r *bufio.Scanner) {
		for r.Scan() {
			log.Printf("[cloudflared] %s", r.Text()) //nolint:gosec // cloudflared output, not user-controlled input
		}
	}
	go pipe(bufio.NewScanner(stdout))
	go pipe(bufio.NewScanner(stderr))

	m.cmd = cmd
	m.running = true
	log.Printf("tunnel: cloudflared started (pid %d)", cmd.Process.Pid)
	return nil
}

// watchAndRestart waits for cloudflared to exit and restarts it on failure.
func (m *Manager) watchAndRestart(ctx context.Context, token string) {
	for {
		m.mu.Lock()
		cmd := m.cmd
		m.mu.Unlock()

		if cmd == nil {
			return
		}

		err := cmd.Wait()

		m.mu.Lock()
		m.running = false
		m.mu.Unlock()

		// Context cancelled means intentional shutdown.
		if ctx.Err() != nil {
			return
		}

		log.Printf("tunnel: cloudflared exited (%v) — restarting in 10s", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}

		m.mu.Lock()
		if err := m.launchCmd(ctx, token); err != nil {
			log.Printf("tunnel: restart failed: %v", err)
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
	}
}

// Stop gracefully shuts down the cloudflared subprocess.
func (m *Manager) Stop() {
	m.mu.Lock()
	cancel := m.cancel
	cmd := m.cmd
	m.cancel = nil
	m.cmd = nil
	m.running = false
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
}
