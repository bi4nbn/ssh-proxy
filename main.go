// SSH Proxy —— 带图形界面的 SSH 代理工具
//
// 启动后会在本机拉起一个仅监听 127.0.0.1 的服务，并打开应用窗口；
// 界面通过 SSE 实时显示连接状态、流量、延迟与日志。
// 程序常驻系统托盘：最小化或关闭窗口都会缩到右下角托盘，代理继续在后台运行。
package main

import (
	"bytes"
	"embed"
	"encoding/json"
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

var version = "1.1.3"

// winTitle 必须与网页 <title> 完全一致，用于找到并接管应用窗口。
const winTitle = "SSH Proxy · SSH 代理工具"

// instanceMutex 是全局单实例锁的名字（winapp 内部会加 Global\/Local\ 前缀）。
const instanceMutex = "SSHProxy.SingleInstance"

var (
	appURL  string
	appLog  *log.Logger
	console io.Writer = io.Discard // 异步的控制台输出，避免 CONOUT$ 阻塞主流程
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

	// 全局只允许一个实例。
	// 互斥体是原子的：两个进程同时启动也只有一个能拿到；进程被强杀时系统自动释放，不留僵尸锁。
	if !winapp.AcquireSingleInstance(instanceMutex) {
		appLog.Printf("已有实例在运行，本次启动只负责把它的窗口唤到前台")
		focusRunning(*addr, *noBrowser)
		return
	}

	// 双保险：旧版本没有互斥体，这里再用 HTTP 探测兜底，避免升级期出现两个实例。
	if url, ok := findRunningInstance(*addr); ok {
		appLog.Printf("检测到已在运行的实例 %s，本次启动只负责唤起窗口", url)
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
	writeInstanceState(actual)

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
	removeInstanceState()
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
	appLog.Printf("托盘菜单命令: id=%d", id)
	switch id {
	case winapp.CmdOpen:
		openUI(appURL)
	case winapp.CmdToggle:
		toggleConnection()
	case winapp.CmdQuit:
		shutdown()
	default:
		appLog.Printf("未识别的托盘命令 id=%d，已忽略", id)
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

// shutdown 退出程序。
//
// 这里每一步都不能假设一定成功：断开 SSH 可能因为对端不响应而卡在 Close 上，
// 移除托盘图标可能因为 Explorer 忙而卡在 Shell_NotifyIcon 上。只要卡住一步，
// 后面的 os.Exit 就永远执行不到，表现就是"点了退出，进程还在"。
// 所以每一步都带超时，另外再挂一个 3 秒看门狗兜底：时间一到无条件退出。
// 进程退出时操作系统会回收全部 socket，本地监听端口不会残留。
func shutdown() {
	if appLog != nil {
		appLog.Printf("收到退出请求，正在退出…")
	}

	// 看门狗：清理动作再慢，也不让进程赖着不走
	go func() {
		time.Sleep(3 * time.Second)
		os.Exit(0)
	}()

	// 先摘托盘图标，让用户立刻看到"退出了"
	quitDone := make(chan struct{})
	go func() {
		winapp.Quit()
		close(quitDone)
	}()
	select {
	case <-quitDone:
	case <-time.After(800 * time.Millisecond):
		if appLog != nil {
			appLog.Printf("移除托盘图标超时，跳过")
		}
	}

	// 再优雅断开，最多等 1.5 秒
	if appMgr != nil {
		disDone := make(chan struct{})
		go func() {
			_ = appMgr.Disconnect()
			close(disDone)
		}()
		select {
		case <-disDone:
		case <-time.After(1500 * time.Millisecond):
			if appLog != nil {
				appLog.Printf("断开连接超时，直接退出（端口随进程释放）")
			}
		}
	}

	removeInstanceState()
	os.Exit(0)
}

/* ---------------- 单实例 ---------------- */

// instanceState 记录当前实例的监听地址，供"第二次启动"找回它。
type instanceState struct {
	PID  int    `json:"pid"`
	Addr string `json:"addr"`
}

func instanceFilePath() string { return filepath.Join(dataDir, "instance.json") }

func writeInstanceState(addr string) {
	b, err := json.Marshal(instanceState{PID: os.Getpid(), Addr: addr})
	if err != nil {
		return
	}
	if err := os.WriteFile(instanceFilePath(), b, 0o644); err != nil {
		appLog.Printf("写入实例信息失败: %v", err)
	}
}

func removeInstanceState() {
	if dataDir == "" {
		return // 还没确定数据目录（启动早期出错），别乱删当前目录下的同名文件
	}
	_ = os.Remove(instanceFilePath())
}

func readInstanceState() (instanceState, bool) {
	var s instanceState
	b, err := os.ReadFile(instanceFilePath())
	if err != nil {
		return s, false
	}
	if err := json.Unmarshal(b, &s); err != nil || s.Addr == "" {
		return s, false
	}
	return s, true
}

// probeInstance 判断某个地址上跑的是不是本程序。
func probeInstance(url string) bool {
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get(url + "/api/meta")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return false
	}
	return strings.Contains(string(b), `"version"`) && strings.Contains(string(b), `"dataDir"`)
}

// findRunningInstance 找出正在运行的那个实例的地址。
// 先看上次写下的 instance.json（端口可能已顺延到 7778~7790），再逐个端口探测。
func findRunningInstance(defaultAddr string) (string, bool) {
	if s, ok := readInstanceState(); ok {
		if u := "http://" + s.Addr; probeInstance(u) {
			return u, true
		}
	}
	if u := "http://" + defaultAddr; probeInstance(u) {
		return u, true
	}
	host, _, err := net.SplitHostPort(defaultAddr)
	if err != nil {
		host = "127.0.0.1"
	}
	for port := 7777; port <= 7790; port++ {
		u := "http://" + net.JoinHostPort(host, fmt.Sprint(port))
		if u == "http://"+defaultAddr {
			continue
		}
		if probeInstance(u) {
			return u, true
		}
	}
	return "", false
}

// focusRunning 把已运行实例的窗口唤到前台（窗口在托盘里也能拉出来）。
func focusRunning(defaultAddr string, noBrowser bool) {
	if noBrowser {
		return
	}
	url, ok := findRunningInstance(defaultAddr)
	if !ok {
		// 锁被占着但探测不到服务，说明对方正在启动或已卡死，等它一会儿
		for i := 0; i < 10 && !ok; i++ {
			time.Sleep(300 * time.Millisecond)
			url, ok = findRunningInstance(defaultAddr)
		}
	}
	if !ok {
		return
	}
	appURL = url
	openUI(url)
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

// asyncWriter 把日志异步写到控制台。
// 双击启动时 os.Stdout 指向的 CONOUT$ 可能没有读者，缓冲区写满后 Write 会一直阻塞，
// 而 log.Logger 内部有锁——一旦卡住，整个程序的日志（以及调用它的协程）都会停摆。
type asyncWriter struct {
	ch chan []byte
}

func newAsyncWriter(w io.Writer) *asyncWriter {
	a := &asyncWriter{ch: make(chan []byte, 256)}
	go func() {
		for b := range a.ch {
			_, _ = w.Write(b)
		}
	}()
	return a
}

func (a *asyncWriter) Write(p []byte) (int, error) {
	b := make([]byte, len(p))
	copy(b, p)
	select {
	case a.ch <- b:
	default: // 队列满就丢弃，绝不阻塞业务协程
	}
	return len(p), nil
}

func initLog() {
	console = newAsyncWriter(os.Stdout)
	f, err := os.OpenFile(filepath.Join(dataDir, "app.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		appLog = log.New(console, "", log.LstdFlags)
		return
	}
	appLog = log.New(io.MultiWriter(f, console), "", log.LstdFlags)
}

func printBanner(url, dir string) {
	line := strings.Repeat("─", 56)
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString("  " + line + "\n")
	fmt.Fprintf(&b, "   SSH Proxy %s   已启动\n", version)
	b.WriteString("  " + line + "\n")
	fmt.Fprintf(&b, "   界面地址 : %s\n", url)
	fmt.Fprintf(&b, "   数据目录 : %s\n", dir)
	b.WriteString("   最小化 / 关闭窗口后缩到右下角托盘继续运行\n")
	b.WriteString("   退出请右键点击托盘图标 -> 退出\n")
	b.WriteString("  " + line + "\n\n")
	_, _ = io.WriteString(console, b.String())
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
	removeInstanceState()
	winapp.MessageBox("SSH Proxy 启动失败", msg)
	os.Exit(1)
}
