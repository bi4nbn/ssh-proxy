// Package api 提供本地 HTTP 接口与静态界面，前端通过 SSE 获取实时状态。
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"sshproxy/internal/sshx"
	"sshproxy/internal/store"
)

// Server 聚合 HTTP 路由。
type Server struct {
	m   *sshx.Manager
	web fs.FS
}

// New 构建带静态资源与 API 的 http.Handler。
func New(m *sshx.Manager, web fs.FS) http.Handler {
	s := &Server{m: m, web: web}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/profiles", s.handleListProfiles)
	mux.HandleFunc("POST /api/profiles", s.handleSaveProfile)
	mux.HandleFunc("DELETE /api/profiles", s.handleDeleteProfile)
	mux.HandleFunc("POST /api/connect", s.handleConnect)
	mux.HandleFunc("POST /api/disconnect", s.handleDisconnect)
	mux.HandleFunc("POST /api/test", s.handleTest)
	mux.HandleFunc("POST /api/trust", s.handleTrust)
	mux.HandleFunc("POST /api/forget", s.handleForget)
	mux.HandleFunc("POST /api/logs/clear", s.handleClearLogs)
	mux.HandleFunc("POST /api/pickfile", s.handlePickFile)
	mux.HandleFunc("GET /api/keys", s.handleKeys)
	mux.HandleFunc("GET /api/meta", s.handleMeta)

	sub, err := fs.Sub(web, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServerFS(sub))
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return errors.New("请求体为空")
	}
	return json.Unmarshal(b, v)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"status":   s.m.Snapshot(),
		"logs":     s.m.Logs(since),
		"profiles": s.m.Store().List(),
		"dataDir":  s.m.Store().Dir(),
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, errors.New("不支持流式响应"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	ctx := r.Context()
	ticker := time.NewTicker(700 * time.Millisecond)
	defer ticker.Stop()

	push := func() bool {
		logs := s.m.Logs(since)
		for _, l := range logs {
			if l.Seq > since {
				since = l.Seq
			}
		}
		payload := map[string]any{
			"status": s.m.Snapshot(),
			"logs":   logs,
		}
		b, err := json.Marshal(payload)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !push() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !push() {
				return
			}
		}
	}
}

func (s *Server) handleListProfiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "profiles": s.m.Store().List()})
}

func (s *Server) handleSaveProfile(w http.ResponseWriter, r *http.Request) {
	var p store.Profile
	if err := decode(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	saved, err := s.m.Store().Upsert(p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "profile": saved})
}

func (s *Server) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, errors.New("缺少 id"))
		return
	}
	st := s.m.Snapshot()
	if st.ProfileID == id && (st.State == sshx.StateConnected || st.State == sshx.StateConnecting) {
		writeErr(w, http.StatusConflict, errors.New("该配置正在使用中，请先断开连接"))
		return
	}
	if err := s.m.Store().Delete(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string        `json:"id"`
		Profile store.Profile `json:"profile"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var err error
	if body.ID != "" {
		err = s.m.Connect(body.ID)
	} else if body.Profile.Host != "" {
		// 直接使用界面上的（可能未保存的）配置连接
		p, serr := s.m.Store().Upsert(body.Profile)
		if serr != nil {
			writeErr(w, http.StatusBadRequest, serr)
			return
		}
		err = s.m.ConnectProfile(p)
	} else {
		writeErr(w, http.StatusBadRequest, errors.New("缺少连接配置"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.m.Disconnect(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTest(w http.ResponseWriter, r *http.Request) {
	var p store.Profile
	if err := decode(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.m.Test(p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res["ok"] = true
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleTrust(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Addr string `json:"addr"`
	}
	_ = decode(r, &body)
	if body.Addr == "" {
		writeErr(w, http.StatusBadRequest, errors.New("缺少地址"))
		return
	}
	if err := s.m.Trust(body.Addr); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleForget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Addr string `json:"addr"`
	}
	_ = decode(r, &body)
	if err := s.m.Forget(body.Addr); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	s.m.ClearLogs()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"os":      runtime.GOOS,
		"arch":    runtime.GOARCH,
		"dataDir": s.m.Store().Dir(),
		"version": "1.0.0",
	})
}

// handleKeys 扫描 ~/.ssh 下常见的私钥文件，方便界面一键选择。
func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".ssh")
	entries, err := os.ReadDir(dir)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": []string{}})
		return
	}
	var keys []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, ".pub") || strings.Contains(lower, "known_hosts") || strings.HasPrefix(lower, "config") {
			continue
		}
		full := filepath.Join(dir, name)
		b, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), "PRIVATE KEY") {
			keys = append(keys, full)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": keys})
}
