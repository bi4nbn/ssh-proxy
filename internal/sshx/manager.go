// Package sshx 是应用核心：维护 SSH 连接、SOCKS5 代理、端口转发、实时统计与日志。
package sshx

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"sshproxy/internal/socks"
	"sshproxy/internal/store"
)

// 连接状态常量。
const (
	StateIdle          = "idle"
	StateConnecting    = "connecting"
	StateConnected     = "connected"
	StateDisconnecting = "disconnecting"
	StateError         = "error"
	StateReconnecting  = "reconnecting"
)

// LogEntry 是一条界面日志。
type LogEntry struct {
	Seq   int64  `json:"seq"`
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// Status 是推送给界面的实时快照。
type Status struct {
	State       string   `json:"state"`
	ProfileID   string   `json:"profileId"`
	ProfileName string   `json:"profileName"`
	Host        string   `json:"host"`
	Addr        string   `json:"addr"`
	User        string   `json:"user"`
	Since       int64    `json:"since"`   // 连接建立时间（毫秒时间戳）
	Uptime      int64    `json:"uptime"`  // 已连接秒数
	BytesIn     uint64   `json:"bytesIn"` // 下行（服务器 -> 本地）
	BytesOut    uint64   `json:"bytesOut"`
	Latency     float64  `json:"latency"` // 毫秒；-1 表示超时，0 表示未知
	Fingerprint string   `json:"fingerprint"`
	AuthType    string   `json:"authType"`
	SocksAddr   string   `json:"socksAddr"`
	Forwards    []string `json:"forwards"`
	Error       string   `json:"error"`
	Attempt     int      `json:"attempt"`
	Trusted     bool     `json:"trusted"`
}

// Manager 管理唯一的活动连接。
type Manager struct {
	mu    sync.Mutex
	st    *store.Store
	state string
	prof  *store.Profile
	gen   int

	client   *ssh.Client
	rawConn  net.Conn
	started  time.Time
	lastErr  string
	fp       string
	socksSrv *socks.Server
	socksAdr string
	fwdLns   []net.Listener
	fwdDesc  []string
	cancel   chan struct{}
	attempt  int

	bytesIn  atomic.Uint64
	bytesOut atomic.Uint64
	latency  atomic.Int64

	logMu  sync.Mutex
	logs   []LogEntry
	logSeq int64
}

// New 创建管理器。
func New(st *store.Store) *Manager {
	m := &Manager{st: st, state: StateIdle, logs: make([]LogEntry, 0, 256)}
	m.logf("info", "SSH Proxy 已启动，配置目录: %s", st.Dir())
	return m
}

// Snapshot 返回当前状态快照。
func (m *Manager) Snapshot() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{
		State:       m.state,
		Error:       m.lastErr,
		Fingerprint: m.fp,
		SocksAddr:   m.socksAdr,
		Attempt:     m.attempt,
		BytesIn:     m.bytesIn.Load(),
		BytesOut:    m.bytesOut.Load(),
		Latency:     microToMs(m.latency.Load()),
	}
	if m.prof != nil {
		s.ProfileID = m.prof.ID
		s.ProfileName = m.prof.Name
		s.Host = m.prof.Host
		s.User = m.prof.User
		s.Addr = m.prof.Addr()
		s.AuthType = m.prof.AuthType
		s.Trusted = m.st.KnownHost(m.prof.Addr()) != "" && m.st.KnownHost(m.prof.Addr()) == m.fp
	}
	if !m.started.IsZero() {
		s.Since = m.started.UnixMilli()
		s.Uptime = int64(time.Since(m.started).Seconds())
	}
	if len(m.fwdDesc) > 0 {
		s.Forwards = append([]string(nil), m.fwdDesc...)
	}
	return s
}

// Logs 返回 seq 之后的新日志。
func (m *Manager) Logs(since int64) []LogEntry {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	out := make([]LogEntry, 0, 16)
	for _, e := range m.logs {
		if e.Seq > since {
			out = append(out, e)
		}
	}
	return out
}

// ClearLogs 清空日志缓冲。
func (m *Manager) ClearLogs() {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	m.logs = m.logs[:0]
}

func (m *Manager) logf(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	m.logMu.Lock()
	m.logSeq++
	e := LogEntry{Seq: m.logSeq, Time: time.Now().Format("15:04:05"), Level: level, Msg: msg}
	m.logs = append(m.logs, e)
	if len(m.logs) > 400 {
		m.logs = append([]LogEntry(nil), m.logs[len(m.logs)-400:]...)
	}
	m.logMu.Unlock()
}

// Connect 使用指定配置建立连接（异步）。
func (m *Manager) Connect(id string) error {
	p, ok := m.st.Get(id)
	if !ok {
		return errors.New("配置不存在")
	}
	return m.ConnectProfile(p)
}

// ConnectProfile 直接使用给定配置连接。
func (m *Manager) ConnectProfile(p store.Profile) error {
	p.Normalize()
	m.mu.Lock()
	if m.state == StateConnecting || m.state == StateConnected || m.state == StateReconnecting {
		m.mu.Unlock()
		return errors.New("当前已有连接，请先断开")
	}
	m.gen++
	gen := m.gen
	m.prof = &p
	m.state = StateConnecting
	m.lastErr = ""
	m.fp = ""
	m.started = time.Time{}
	m.socksAdr = ""
	m.fwdDesc = nil
	m.attempt = 1
	m.cancel = make(chan struct{})
	cancel := m.cancel
	m.mu.Unlock()

	m.bytesIn.Store(0)
	m.bytesOut.Store(0)
	m.latency.Store(0)

	go m.run(gen, p, cancel)
	return nil
}

// Disconnect 主动断开连接并停止所有监听。
func (m *Manager) Disconnect() error {
	m.mu.Lock()
	if m.state == StateIdle && m.client == nil && m.socksSrv == nil {
		m.mu.Unlock()
		return nil
	}
	m.gen++
	if m.cancel != nil {
		close(m.cancel)
		m.cancel = nil
	}
	srv := m.socksSrv
	m.socksSrv = nil
	lns := m.fwdLns
	m.fwdLns = nil
	client := m.client
	m.state = StateDisconnecting
	m.mu.Unlock()

	if srv != nil {
		_ = srv.Close()
	}
	for _, ln := range lns {
		_ = ln.Close()
	}
	if client != nil {
		_ = client.Close()
	}

	m.mu.Lock()
	m.client = nil
	m.rawConn = nil
	m.started = time.Time{}
	m.socksAdr = ""
	m.fwdDesc = nil
	m.attempt = 0
	m.state = StateIdle
	m.mu.Unlock()
	m.latency.Store(0)
	m.logf("info", "连接已断开")
	return nil
}

func (m *Manager) isCurrent(gen int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gen == gen
}

func (m *Manager) run(gen int, p store.Profile, cancel chan struct{}) {
	backoff := 2 * time.Second
	for attempt := 1; ; attempt++ {
		m.setAttempt(attempt)
		err := m.dialAndServe(gen, p, cancel, attempt)
		if !m.isCurrent(gen) {
			return
		}
		m.mu.Lock()
		m.client = nil
		m.rawConn = nil
		m.started = time.Time{}
		m.socksAdr = ""
		m.fwdDesc = nil
		m.state = StateError
		m.lastErr = friendlyError(err)
		m.mu.Unlock()
		m.latency.Store(0)

		m.logf("error", "连接中断: %s", friendlyError(err))
		m.logf("debug", "原始错误: %v", err)
		if !p.AutoReconnect {
			return
		}
		if attempt >= 8 {
			m.logf("error", "自动重连已达上限（8 次），已停止")
			return
		}
		m.mu.Lock()
		if m.gen == gen {
			m.state = StateReconnecting
		}
		m.mu.Unlock()
		m.logf("warn", "%s 后自动重连（第 %d 次）...", backoff, attempt)

		select {
		case <-cancel:
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (m *Manager) setAttempt(n int) {
	m.mu.Lock()
	if m.attempt != n {
		m.attempt = n
	}
	m.mu.Unlock()
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// friendlyError 把底层错误翻译成对使用者友好的提示。
func friendlyError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	lower := strings.ToLower(s)
	switch {
	case strings.Contains(s, "认证失败") || strings.Contains(s, "主机密钥") || strings.Contains(s, "主机指纹"):
		return s // 已是中文提示，保留细节
	case strings.Contains(s, "unable to authenticate"):
		return "认证失败：用户名、密码或私钥不正确"
	case strings.Contains(s, "no supported methods remain"):
		return "认证失败：服务器拒绝了当前认证方式"
	case strings.Contains(s, "no connection could be made") || strings.Contains(s, "connection refused"):
		return "无法连接：目标端口未开放，或被防火墙拦截"
	case strings.Contains(s, "i/o timeout") || strings.Contains(s, "deadline exceeded"):
		return "连接超时：请检查主机地址、端口与网络是否可达"
	case strings.Contains(lower, "no such host"):
		return "域名解析失败：请检查主机地址"
	case strings.Contains(s, "handshake failed"):
		return "SSH 握手失败：" + strings.TrimPrefix(s, "SSH 握手失败: ")
	case strings.Contains(s, "EOF") || strings.Contains(s, "connection reset"):
		return "连接被对端关闭"
	}
	return s
}

// dialAndServe 建立一次连接并阻塞到连接结束。
func (m *Manager) dialAndServe(gen int, p store.Profile, cancel chan struct{}, attempt int) error {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		select {
		case <-cancel:
			stop()
		case <-ctx.Done():
		}
	}()

	cfg, err := m.buildConfig(p, nil)
	if err != nil {
		return err
	}

	timeout := time.Duration(p.Timeout) * time.Second
	d := net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	m.logf("info", "正在连接 %s@%s（第 %d 次尝试）...", p.User, p.Addr(), attempt)

	raw, err := d.DialContext(ctx, "tcp", p.Addr())
	if err != nil {
		return fmt.Errorf("无法连接 %s: %w", p.Addr(), err)
	}

	conn, chans, reqs, err := ssh.NewClientConn(raw, p.Addr(), cfg)
	if err != nil {
		_ = raw.Close()
		return fmt.Errorf("SSH 握手失败: %w", err)
	}
	client := ssh.NewClient(conn, chans, reqs)

	m.mu.Lock()
	m.client = client
	m.rawConn = raw
	m.started = time.Now()
	m.state = StateConnected
	m.lastErr = ""
	m.mu.Unlock()

	m.bytesIn.Store(0)
	m.bytesOut.Store(0)
	m.latency.Store(0)
	m.logf("success", "已连接 %s@%s", p.User, p.Addr())

	known := m.st.KnownHost(p.Addr())
	switch {
	case p.StrictHostKey:
		m.logf("info", "主机指纹已校验: %s", m.fp)
	case known == "":
		_ = m.st.TrustHost(p.Addr(), m.fp)
		m.logf("info", "已记录主机指纹: %s", m.fp)
	case known != m.fp:
		_ = m.st.TrustHost(p.Addr(), m.fp)
		m.logf("warn", "主机指纹已变化（未启用严格校验），已更新记录: %s", m.fp)
	default:
		m.logf("info", "主机指纹一致: %s", m.fp)
	}

	// 端口转发
	for _, f := range p.Forwards {
		if f.LocalPort == 0 || f.RemoteHost == "" || f.RemotePort == 0 {
			continue
		}
		if err := m.startForward(client, f); err != nil {
			m.logf("warn", "端口转发 %d -> %s:%d 启动失败: %v", f.LocalPort, f.RemoteHost, f.RemotePort, err)
		}
	}

	// SOCKS5 代理
	if p.SocksEnabled {
		srv, err := socks.New(fmt.Sprintf("127.0.0.1:%d", p.SocksPort), m.countingDialer(client), func(level, msg string) {
			m.logf(level, "%s", msg)
		})
		if err != nil {
			m.logf("error", "SOCKS5 代理监听 127.0.0.1:%d 失败: %v", p.SocksPort, err)
			m.mu.Lock()
			m.lastErr = "SOCKS5 监听失败: " + err.Error()
			m.mu.Unlock()
		} else {
			m.mu.Lock()
			m.socksSrv = srv
			m.socksAdr = srv.Addr()
			m.mu.Unlock()
			m.logf("success", "SOCKS5 代理已就绪: socks5://%s", srv.Addr())
		}
	}

	// 心跳 + 延迟探测
	pingCtx, pingStop := context.WithCancel(context.Background())
	defer pingStop()
	go m.pinger(pingCtx, client, p.KeepAlive)

	done := make(chan struct{})
	go func() {
		_ = client.Wait()
		close(done)
	}()

	select {
	case <-cancel:
		m.cleanupListeners()
		_ = client.Close()
		return nil
	case <-done:
		m.cleanupListeners()
		return errors.New("SSH 连接已断开")
	}
}

func (m *Manager) cleanupListeners() {
	m.mu.Lock()
	srv := m.socksSrv
	m.socksSrv = nil
	lns := m.fwdLns
	m.fwdLns = nil
	m.socksAdr = ""
	m.fwdDesc = nil
	m.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
	for _, ln := range lns {
		_ = ln.Close()
	}
}

func (m *Manager) pinger(ctx context.Context, client *ssh.Client, interval int) {
	iv := time.Duration(interval) * time.Second
	if iv < 5*time.Second {
		iv = 5 * time.Second
	}
	if iv > 60*time.Second {
		iv = 60 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			type res struct {
				d   time.Duration
				err error
			}
			ch := make(chan res, 1)
			go func() {
				st := time.Now()
				_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
				ch <- res{time.Since(st), err}
			}()
			select {
			case r := <-ch:
				if r.err != nil {
					return
				}
				m.latency.Store(r.d.Microseconds())
			case <-time.After(iv):
				m.latency.Store(-1) // 超时
			case <-ctx.Done():
				return
			}
		}
	}
}

// buildConfig 构造 SSH 客户端配置；fpOut 非空时指纹写入该指针（测试连接用），
// 为空时写入当前连接状态（正式连接用）。
func (m *Manager) buildConfig(p store.Profile, fpOut *string) (*ssh.ClientConfig, error) {
	var auths []ssh.AuthMethod
	switch p.AuthType {
	case "password":
		if p.Password == "" {
			return nil, errors.New("密码为空")
		}
		auths = append(auths, ssh.Password(p.Password))
		auths = append(auths, ssh.KeyboardInteractive(func(name, instruction string, qs []string, echos []bool) ([]string, error) {
			out := make([]string, len(qs))
			for i := range qs {
				out[i] = p.Password
			}
			return out, nil
		}))
	case "key", "both":
		key, err := loadKey(p)
		if err != nil {
			return nil, err
		}
		auths = append(auths, ssh.PublicKeys(key))
		if p.AuthType == "both" && p.Password != "" {
			auths = append(auths, ssh.Password(p.Password))
		}
	default:
		if p.Password != "" {
			auths = append(auths, ssh.Password(p.Password))
		} else {
			key, err := loadKey(p)
			if err != nil {
				return nil, err
			}
			auths = append(auths, ssh.PublicKeys(key))
		}
	}
	if len(auths) == 0 {
		return nil, errors.New("没有可用的认证方式")
	}

	addr := p.Addr()
	cfg := &ssh.ClientConfig{
		User: p.User,
		Auth: auths,
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			fp := fingerprint(key)
			if fpOut != nil {
				*fpOut = fp
			} else {
				m.mu.Lock()
				m.fp = fp
				m.mu.Unlock()
			}
			known := m.st.KnownHost(addr)
			if p.StrictHostKey {
				if known == "" {
					return fmt.Errorf("主机密钥未受信任（%s），请先在设置中点击「信任此主机」", fp)
				}
				if known != fp {
					return fmt.Errorf("主机密钥已变更！\n已保存: %s\n当前值: %s\n如确认安全，请清除并重新信任该主机", known, fp)
				}
			}
			return nil
		},
		Timeout: time.Duration(p.Timeout) * time.Second,
	}
	return cfg, nil
}

func loadKey(p store.Profile) (ssh.Signer, error) {
	var data []byte
	var err error
	if p.KeyPath != "" {
		data, err = os.ReadFile(p.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("读取私钥失败: %w", err)
		}
	} else if p.KeyData != "" {
		data = []byte(p.KeyData)
	} else {
		return nil, errors.New("未提供私钥")
	}
	if p.KeyPassphrase != "" {
		k, err := ssh.ParsePrivateKeyWithPassphrase(data, []byte(p.KeyPassphrase))
		if err != nil {
			return nil, fmt.Errorf("解析私钥失败（口令可能不正确）: %w", err)
		}
		return k, nil
	}
	k, err := ssh.ParsePrivateKey(data)
	if err != nil {
		if strings.Contains(err.Error(), "passphrase") {
			return nil, errors.New("私钥已加密，请填写私钥口令")
		}
		return nil, fmt.Errorf("解析私钥失败: %w", err)
	}
	return k, nil
}

// microToMs 把内部记录的微秒换算成毫秒（保留小数，避免本机回环显示为 0）。
func microToMs(us int64) float64 {
	if us == 0 || us < 0 {
		return float64(us)
	}
	return math.Round(float64(us)/100) / 10
}

func fingerprint(key ssh.PublicKey) string {
	sum := sha256.Sum256(key.Marshal())
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// countingConn 统计代理流量。
type countingConn struct {
	net.Conn
	in  *atomic.Uint64
	out *atomic.Uint64
}

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if n > 0 {
		c.in.Add(uint64(n))
	}
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.out.Add(uint64(n))
	}
	return n, err
}

func (m *Manager) countingDialer(client *ssh.Client) socks.Dialer {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := client.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &countingConn{Conn: c, in: &m.bytesIn, out: &m.bytesOut}, nil
	}
}

func (m *Manager) startForward(client *ssh.Client, f store.Forward) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", f.LocalPort))
	if err != nil {
		return err
	}
	desc := fmt.Sprintf("127.0.0.1:%d -> %s:%d", f.LocalPort, f.RemoteHost, f.RemotePort)
	m.mu.Lock()
	m.fwdLns = append(m.fwdLns, ln)
	m.fwdDesc = append(m.fwdDesc, desc)
	m.mu.Unlock()
	m.logf("success", "端口转发已就绪: %s", desc)

	dial := m.countingDialer(client)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(local net.Conn) {
				defer local.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				remote, err := dial(ctx, "tcp", net.JoinHostPort(f.RemoteHost, fmt.Sprint(f.RemotePort)))
				cancel()
				if err != nil {
					m.logf("warn", "转发 %d -> %s:%d 失败: %v", f.LocalPort, f.RemoteHost, f.RemotePort, err)
					return
				}
				defer remote.Close()
				done := make(chan struct{}, 2)
				go func() { _, _ = copyTo(remote, local); done <- struct{}{} }()
				go func() { _, _ = copyTo(local, remote); done <- struct{}{} }()
				<-done
			}(c)
		}
	}()
	return nil
}

// Test 使用给定配置做一次快速探测（不影响当前连接）。
func (m *Manager) Test(p store.Profile) (map[string]any, error) {
	p.Normalize()
	if p.Host == "" {
		return nil, errors.New("主机地址不能为空")
	}
	var fp string
	cfg, err := m.buildConfig(p, &fp)
	if err != nil {
		return nil, err
	}
	d := net.Dialer{Timeout: time.Duration(p.Timeout) * time.Second}
	start := time.Now()
	raw, err := d.Dial("tcp", p.Addr())
	if err != nil {
		return nil, fmt.Errorf("TCP 连接失败: %w", err)
	}
	defer raw.Close()
	tcpMs := time.Since(start).Milliseconds()

	conn, chans, reqs, err := ssh.NewClientConn(raw, p.Addr(), cfg)
	if err != nil {
		return nil, fmt.Errorf("SSH 握手失败: %w", err)
	}
	client := ssh.NewClient(conn, chans, reqs)
	defer client.Close()

	hs := time.Since(start).Milliseconds()
	st := time.Now()
	_, _, _ = client.SendRequest("keepalive@openssh.com", true, nil)
	lat := time.Since(st).Milliseconds()

	trusted := m.st.KnownHost(p.Addr())
	return map[string]any{
		"ok":          true,
		"tcpMs":       tcpMs,
		"handshakeMs": hs,
		"latencyMs":   microToMs(lat * 1000),
		"fingerprint": fp,
		"trusted":     trusted != "" && trusted == fp,
		"server":      string(conn.ServerVersion()),
	}, nil
}

// Trust 记录当前主机指纹。
func (m *Manager) Trust(addr string) error {
	m.mu.Lock()
	fp := m.fp
	m.mu.Unlock()
	if fp == "" {
		return errors.New("尚未获取到主机指纹，请先测试或连接一次")
	}
	return m.st.TrustHost(addr, fp)
}

// Forget 清除主机指纹。
func (m *Manager) Forget(addr string) error { return m.st.ForgetHost(addr) }

// CurrentProfileID 返回最近一次使用（或正在使用）的配置 ID。
func (m *Manager) CurrentProfileID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.prof != nil {
		return m.prof.ID
	}
	return ""
}

// Store 暴露底层存储。
func (m *Manager) Store() *store.Store { return m.st }
