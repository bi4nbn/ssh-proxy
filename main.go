// SSH Proxy —— 带图形界面的 SSH 代理工具
//
// 启动后会在本机拉起一个仅监听 127.0.0.1 的服务，并打开应用窗口；
// 界面通过 SSE 实时显示连接状态、流量、延迟与日志。
// 程序常驻系统托盘：最小化或关闭窗口都会缩到右下角托盘，代理继续在后台运行。
package main

import (
	"bytes"
	"embed"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"sshproxy/internal/api"
	"sshproxy/internal/sshx"
	"sshproxy/internal/store"
	"sshproxy/internal/winapp"
)

//go:embed all:web
var embeddedWeb embed.FS

//go:embed build/app.ico
var appIconICO []byte

var version = "1.1.1"

// winTitle 必须与网页 <title> 完全一致，用于找到并接管应用窗口。
const winTitle = "SSH Proxy · SSH 代理工具"

var (
	appURL  string
	appLog  *log.Logger
	appMgr  *sshx.Manager
	dataDir string
)

func main() {
	var (
		addr      = flag.String("addr", "127.0.0.1:7777", "监听地址")
		noBrowser = flag.Bool("no-browser", false, "不自动打开界面")
		showVer   = flag.Bool("v", false, "显示版本号")
	)
	flag.Parse()

	// 以 GUI 子系统编译（无控制台窗口）；从终端启动时挂到父控制台，日志照常输出
	winapp.AttachParentConsole()

	// 必须尽早开启：否则高 DPI 下取到的图标尺寸会被系统虚拟化，任务栏图标会被二次放大而发虚
	winapp.EnableDPIAwareness()

	if *showVer {
		fmt.Printf("ssh-proxy %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	st, err := store.Open()
	if err != nil {
		fatal("初始化配置目录失败: %v", err)
	}
	dataDir = st.Dir()
	initLog()
	appMgr = sshx.New(st)

	// 已有实例在运行时，直接唤起它的窗口
	if url, ok := detectRunning(*addr); ok {
		appURL = url
		if !*noBrowser {
			openUI(url)
		}
		return
	}

	ln, actual, err := listen(*addr)
	if err != nil {
		fatal("端口监听失败: %v", err)
	}
	appURL = "http://" + actual

	// 双击启动时隐藏自带的控制台窗口，实现后台静默运行
	winapp.HideOwnConsole()

	if err := winapp.Start(winapp.Options{
		IconPath:  ensureIconFile(dataDir),
		Tooltip:   trayTooltip,
		Menu:      trayMenu,
		OnCommand: onTrayCommand,
		OnOpen:    func() { openUI(appURL) },
		Logf:      func(f string, a ...any) { appLog.Printf(f, a...) },
	}, winTitle); err != nil {
		appLog.Printf("托盘初始化失败: %v", err)
	}

	printBanner(appURL, dataDir)

	if !*noBrowser {
		go openUI(appURL)
	}

	api.Version = version
	srv := &http.Server{
		Handler:           api.New(appMgr, embeddedWeb),
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fatal("服务异常退出: %v", err)
	}
}

/* ---------------- 托盘 ---------------- */

func trayTooltip() string {
	s := appMgr.Snapshot()
	switch s.State {
	case sshx.StateConnected:
		return fmt.Sprintf("SSH Proxy · 已连接 %s@%s", s.User, s.Addr)
	case sshx.StateConnecting:
		return "SSH Proxy · 正在连接…"
	case sshx.StateReconnecting:
		return "SSH Proxy · 重连中…"
	case sshx.StateError:
		return "SSH Proxy · 连接失败"
	default:
		return "SSH Proxy · 未连接（双击打开界面）"
	}
}

func trayMenu() []winapp.MenuItem {
	s := appMgr.Snapshot()
	toggle := "连接"
	switch s.State {
	case sshx.StateConnected:
		toggle = "断开连接"
	case sshx.StateConnecting, sshx.StateReconnecting:
		toggle = "取消连接"
	}
	return []winapp.MenuItem{
		{ID: winapp.CmdOpen, Label: "打开界面", Default: true},
		{ID: winapp.CmdToggle, Label: toggle},
		{Separator: true},
		{ID: winapp.CmdQuit, Label: "退出"},
	}
}

func onTrayCommand(id int) {
	switch id {
	case winapp.CmdOpen:
		openUI(appURL)
	case winapp.CmdToggle:
		toggleConnection()
	case winapp.CmdQuit:
		shutdown()
	}
}

// toggleConnection 供托盘菜单使用：已连接则断开，否则连接最近一次使用的配置。
func toggleConnection() {
	s := appMgr.Snapshot()
	if s.State == sshx.StateConnected || s.State == sshx.StateConnecting || s.State == sshx.StateReconnecting {
		if err := appMgr.Disconnect(); err != nil {
			winapp.Notify("SSH Proxy", "断开失败: "+err.Error())
			return
		}
		winapp.Notify("SSH Proxy", "已断开连接")
		return
	}
	id := appMgr.CurrentProfileID()
	if id == "" {
		list := appMgr.Store().List()
		if len(list) == 0 {
			winapp.Notify("SSH Proxy", "还没有配置，请先在界面中新建连接")
			openUI(appURL)
			return
		}
		id = list[0].ID
	}
	if err := appMgr.Connect(id); err != nil {
		winapp.Notify("SSH Proxy", err.Error())
	}
}

func shutdown() {
	_ = appMgr.Disconnect()
	winapp.Quit()
	time.Sleep(200 * time.Millisecond)
	os.Exit(0)
}

/* ---------------- 启动辅助 ---------------- */

// detectRunning 探测指定地址上是否已经有本程序的实例在运行。
func detectRunning(addr string) (string, bool) {
	url := "http://" + addr
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get(url + "/api/meta")
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return "", false
	}
	if strings.Contains(string(b), `"version"`) && strings.Contains(string(b), `"dataDir"`) {
		return url, true
	}
	return "", false
}

// listen 依次尝试候选端口，返回实际监听地址。
func listen(addr string) (net.Listener, string, error) {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln, ln.Addr().String(), nil
	}
	host, _, splitErr := net.SplitHostPort(addr)
	if splitErr != nil {
		host = "127.0.0.1"
	}
	for port := 7778; port <= 7790; port++ {
		candidate := net.JoinHostPort(host, fmt.Sprint(port))
		if l, e := net.Listen("tcp", candidate); e == nil {
			return l, l.Addr().String(), nil
		}
	}
	return nil, "", err
}

// ensureIconFile 把内嵌的 ico 释放到数据目录，供 Win32 加载。
func ensureIconFile(dir string) string {
	path := filepath.Join(dir, "app.ico")
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, appIconICO) {
		return path
	}
	if err := os.WriteFile(path, appIconICO, 0o644); err != nil {
		appLog.Printf("写入图标文件失败: %v", err)
	}
	return path
}

func initLog() {
	f, err := os.OpenFile(filepath.Join(dataDir, "app.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		appLog = log.New(io.Discard, "", 0)
		return
	}
	appLog = log.New(io.MultiWriter(os.Stdout, f), "", log.LstdFlags)
}

func printBanner(url, dir string) {
	line := strings.Repeat("─", 56)
	fmt.Println()
	fmt.Println("  " + line)
	fmt.Printf("   SSH Proxy %s   已启动\n", version)
	fmt.Println("  " + line)
	fmt.Printf("   界面地址 : %s\n", url)
	fmt.Printf("   数据目录 : %s\n", dir)
	fmt.Println("   最小化 / 关闭窗口后缩到右下角托盘继续运行")
	fmt.Println("   退出请右键点击托盘图标 -> 退出")
	fmt.Println("  " + line)
	fmt.Println()
}

// openUI 优先唤起已存在的窗口，其次用 Edge/Chrome 的应用模式新开一个。
func openUI(url string) {
	if winapp.FocusExisting(winTitle) {
		return
	}
	time.Sleep(300 * time.Millisecond)
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("LocalAppData"), `Google\Chrome\Application\chrome.exe`),
	}
	profileDir := filepath.Join(dataDir, "uicache")
	for _, exe := range candidates {
		if exe == "" || strings.HasPrefix(exe, string(os.PathSeparator)) {
			continue
		}
		if _, err := os.Stat(exe); err != nil {
			continue
		}
		cmd := exec.Command(exe,
			"--app="+url,
			"--window-size=1320,900",
			"--user-data-dir="+profileDir,
			"--no-first-run",
			"--no-default-browser-check",
		)
		if err := cmd.Start(); err == nil {
			appLog.Printf("已启动界面进程 pid=%d", cmd.Process.Pid)
			return
		}
	}
	openDefault(url)
}

func fatal(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if appLog != nil {
		appLog.Printf("[错误] %s", msg)
	}
	fmt.Fprintf(os.Stderr, "\n  [错误] %s\n\n", msg)
	winapp.MessageBox("SSH Proxy 启动失败", msg)
	os.Exit(1)
}
