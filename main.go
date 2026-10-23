// SSH Proxy —— 带图形界面的 SSH 代理工具
//
// 启动后会在本机拉起一个仅监听 127.0.0.1 的服务，并自动打开应用窗口，
// 界面通过 SSE 实时显示连接状态、流量、延迟与日志。
package main

import (
	"embed"
	"flag"
	"fmt"
	"io"
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
)

//go:embed all:web
var embeddedWeb embed.FS

var version = "1.0.0"

func main() {
	var (
		addr      = flag.String("addr", "127.0.0.1:7777", "监听地址")
		noBrowser = flag.Bool("no-browser", false, "不自动打开界面")
		showVer   = flag.Bool("v", false, "显示版本号")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("ssh-proxy %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
		return
	}

	st, err := store.Open()
	if err != nil {
		fatal("初始化配置目录失败: %v", err)
	}
	mgr := sshx.New(st)

	// 已有实例在运行时，直接复用它的窗口，避免重复启动造成端口冲突
	if url, ok := detectRunning(*addr); ok {
		fmt.Printf("  检测到已运行的实例：%s\n", url)
		if !*noBrowser {
			openUI(url)
			time.Sleep(1500 * time.Millisecond)
		}
		return
	}

	ln, actual, err := listen(*addr)
	if err != nil {
		fatal("端口监听失败: %v", err)
	}

	srv := &http.Server{
		Handler:           api.New(mgr, embeddedWeb),
		ReadHeaderTimeout: 10 * time.Second,
	}

	url := "http://" + actual
	printBanner(url, st.Dir())

	if !*noBrowser {
		go openUI(url)
	}

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fatal("服务异常退出: %v", err)
	}
}

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

func printBanner(url, dataDir string) {
	line := strings.Repeat("─", 56)
	fmt.Println()
	fmt.Println("  " + line)
	fmt.Printf("   SSH Proxy %s   已启动\n", version)
	fmt.Println("  " + line)
	fmt.Printf("   界面地址 : %s\n", url)
	fmt.Printf("   数据目录 : %s\n", dataDir)
	fmt.Println("   关闭本窗口即可退出程序")
	fmt.Println("  " + line)
	fmt.Println()
}

// openUI 优先用 Edge/Chrome 的应用模式打开，获得接近原生窗口的体验。
func openUI(url string) {
	time.Sleep(400 * time.Millisecond)
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Microsoft\Edge\Application\msedge.exe`),
		filepath.Join(os.Getenv("ProgramFiles"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), `Google\Chrome\Application\chrome.exe`),
		filepath.Join(os.Getenv("LocalAppData"), `Google\Chrome\Application\chrome.exe`),
	}
	profileDir := filepath.Join(store.DataDir(), "uicache")
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
			return
		}
	}
	openDefault(url)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n  [错误] "+format+"\n\n", args...)
	fmt.Println("  按回车键退出...")
	_, _ = fmt.Scanln()
	os.Exit(1)
}
