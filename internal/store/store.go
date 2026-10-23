// Package store 负责配置档案（Profile）与主机指纹的本地持久化。
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Forward 描述一条本地端口转发规则：本地端口 -> 远端地址。
type Forward struct {
	LocalPort  int    `json:"localPort"`
	RemoteHost string `json:"remoteHost"`
	RemotePort int    `json:"remotePort"`
	Comment    string `json:"comment,omitempty"`
}

// Profile 是一条完整的连接配置。
type Profile struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Host          string    `json:"host"`
	Port          int       `json:"port"`
	User          string    `json:"user"`
	AuthType      string    `json:"authType"` // password | key | both
	Password      string    `json:"password"`
	KeyPath       string    `json:"keyPath"`
	KeyData       string    `json:"keyData"` // 直接粘贴的私钥内容（优先于 KeyPath）
	KeyPassphrase string    `json:"keyPassphrase"`
	SocksEnabled  bool      `json:"socksEnabled"`
	SocksPort     int       `json:"socksPort"`
	Forwards      []Forward `json:"forwards"`
	KeepAlive     int       `json:"keepAlive"` // 秒
	Timeout       int       `json:"timeout"`   // 秒
	AutoReconnect bool      `json:"autoReconnect"`
	StrictHostKey bool      `json:"strictHostKey"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// Addr 返回 host:port 形式的目标地址。
func (p Profile) Addr() string {
	port := p.Port
	if port <= 0 {
		port = 22
	}
	return joinHostPort(p.Host, port)
}

func joinHostPort(host string, port int) string {
	return host + ":" + itoa(port)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

// Normalize 补齐默认值，避免界面传入零值导致行为异常。
func (p *Profile) Normalize() {
	p.Name = strings.TrimSpace(p.Name)
	p.Host = strings.TrimSpace(p.Host)
	p.User = strings.TrimSpace(p.User)
	p.KeyPath = strings.TrimSpace(p.KeyPath)
	if p.Port <= 0 || p.Port > 65535 {
		p.Port = 22
	}
	if p.AuthType != "key" && p.AuthType != "both" {
		if p.AuthType == "password" {
			p.AuthType = "password"
		} else if p.KeyPath != "" || p.KeyData != "" {
			p.AuthType = "key"
		} else {
			p.AuthType = "password"
		}
	}
	if p.SocksPort <= 0 || p.SocksPort > 65535 {
		p.SocksPort = 1080
	}
	if p.KeepAlive <= 0 {
		p.KeepAlive = 15
	}
	if p.Timeout <= 0 {
		p.Timeout = 15
	}
	if p.Name == "" {
		p.Name = p.User + "@" + p.Host
	}
	if p.Forwards == nil {
		p.Forwards = []Forward{}
	}
	for i := range p.Forwards {
		f := &p.Forwards[i]
		f.RemoteHost = strings.TrimSpace(f.RemoteHost)
		if f.LocalPort <= 0 || f.LocalPort > 65535 {
			f.LocalPort = 0
		}
		if f.RemotePort <= 0 || f.RemotePort > 65535 {
			f.RemotePort = 0
		}
	}
}

// Store 是线程安全的 JSON 文件存储。
type Store struct {
	mu       sync.RWMutex
	dir      string
	profiles []Profile
	known    map[string]string // "host:port" -> "SHA256:..."
}

// DataDir 返回应用数据目录。
func DataDir() string {
	if d := os.Getenv("SSHPROXY_HOME"); d != "" {
		return d
	}
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = "."
	}
	return filepath.Join(base, "SSHProxy")
}

// Open 加载（或初始化）本地存储。
func Open() (*Store, error) {
	dir := DataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, known: map[string]string{}}
	_ = readJSON(filepath.Join(dir, "profiles.json"), &s.profiles)
	_ = readJSON(filepath.Join(dir, "known_hosts.json"), &s.known)
	if s.known == nil {
		s.known = map[string]string{}
	}
	return s, nil
}

// Dir 返回数据目录。
func (s *Store) Dir() string { return s.dir }

// KeysDir 返回存放粘贴私钥的目录。
func (s *Store) KeysDir() string { return filepath.Join(s.dir, "keys") }

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// List 返回按名称排序的配置副本。
func (s *Store) List() []Profile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Profile, len(s.profiles))
	copy(out, s.profiles)
	sort.SliceStable(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// Get 按 ID 查找配置。
func (s *Store) Get(id string) (Profile, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.profiles {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// Upsert 新增或更新配置，返回保存后的结果。
func (s *Store) Upsert(p Profile) (Profile, error) {
	p.Normalize()
	if p.Host == "" {
		return p, errors.New("主机地址不能为空")
	}
	if p.User == "" {
		return p, errors.New("用户名不能为空")
	}
	if p.AuthType == "key" || p.AuthType == "both" {
		if p.KeyPath == "" && p.KeyData == "" {
			return p, errors.New("请填写私钥文件路径或粘贴私钥内容")
		}
	}

	// 粘贴的私钥落盘，避免把敏感内容重复塞进 profiles.json
	if p.KeyData != "" {
		path, err := s.persistKey(p.ID, p.KeyData)
		if err != nil {
			return p, err
		}
		p.KeyPath = path
		p.KeyData = ""
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if p.ID == "" {
		p.ID = newID()
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	replaced := false
	for i := range s.profiles {
		if s.profiles[i].ID == p.ID {
			if s.profiles[i].CreatedAt.IsZero() {
				p.CreatedAt = now
			} else {
				p.CreatedAt = s.profiles[i].CreatedAt
			}
			s.profiles[i] = p
			replaced = true
			break
		}
	}
	if !replaced {
		s.profiles = append(s.profiles, p)
	}
	if err := writeJSON(filepath.Join(s.dir, "profiles.json"), s.profiles); err != nil {
		return p, err
	}
	return p, nil
}

// Delete 删除配置。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.profiles[:0]
	for _, p := range s.profiles {
		if p.ID != id {
			out = append(out, p)
		}
	}
	s.profiles = out
	return writeJSON(filepath.Join(s.dir, "profiles.json"), s.profiles)
}

func (s *Store) persistKey(id, data string) (string, error) {
	dir := s.KeysDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := id
	if name == "" {
		name = newID()
	}
	path := filepath.Join(dir, name+".pem")
	content := data
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// KnownHost 返回已记录的主机指纹。
func (s *Store) KnownHost(addr string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.known[addr]
}

// TrustHost 记录主机指纹。
func (s *Store) TrustHost(addr, fingerprint string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.known[addr] = fingerprint
	return writeJSON(filepath.Join(s.dir, "known_hosts.json"), s.known)
}

// ForgetHost 清除主机指纹。
func (s *Store) ForgetHost(addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.known, addr)
	return writeJSON(filepath.Join(s.dir, "known_hosts.json"), s.known)
}

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "p" + itoa(int(time.Now().UnixNano()%1e9))
	}
	return hex.EncodeToString(b[:])
}
