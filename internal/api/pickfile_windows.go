//go:build windows

package api

import (
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"syscall"
)

// handlePickFile 调用 Windows 原生文件选择框，返回绝对路径。
func (s *Server) handlePickFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title  string `json:"title"`
		Filter string `json:"filter"`
	}
	_ = decode(r, &body)
	title := strings.ReplaceAll(body.Title, "'", "")
	if title == "" {
		title = "选择文件"
	}
	filter := strings.ReplaceAll(body.Filter, "'", "")
	if filter == "" {
		filter = "所有文件 (*.*)|*.*"
	}

	script := fmt.Sprintf(`
Add-Type -AssemblyName System.Windows.Forms | Out-Null
$dlg = New-Object System.Windows.Forms.OpenFileDialog
$dlg.Title = '%s'
$dlg.Filter = '%s'
$dlg.CheckFileExists = $true
$dlg.Multiselect = $false
if ($dlg.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { Write-Output $dlg.FileName }
`, title, filter)

	cmd := exec.Command("powershell", "-NoProfile", "-STA", "-WindowStyle", "Hidden", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, errors.New("无法打开文件选择框: "+err.Error()))
		return
	}
	path := strings.TrimSpace(string(out))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path})
}
