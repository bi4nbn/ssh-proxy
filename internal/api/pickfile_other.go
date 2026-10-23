//go:build !windows

package api

import (
	"errors"
	"net/http"
)

func (s *Server) handlePickFile(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, errors.New("当前系统请手动输入文件路径"))
}
