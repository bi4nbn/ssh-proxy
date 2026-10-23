// Package socks 实现一个最小可用的 SOCKS5 服务端（仅 CONNECT，无认证）。
// 所有出站连接都通过注入的 Dial 函数完成，从而复用 SSH 隧道。
package socks

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// Dialer 与 ssh.Client.DialContext 兼容。
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Logger 用于输出调试信息。
type Logger func(level, msg string)

const (
	verSOCKS5  = 0x05
	cmdConnect = 0x01
	atypIPv4   = 0x01
	atypDomain = 0x03
	atypIPv6   = 0x04
)

// Server 是 SOCKS5 监听器。
type Server struct {
	ln     net.Listener
	dial   Dialer
	log    Logger
	once   sync.Once
	closed chan struct{}
}

// New 创建并开始监听 addr（如 127.0.0.1:1080）。
func New(addr string, dial Dialer, log Logger) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = func(string, string) {}
	}
	s := &Server{ln: ln, dial: dial, log: log, closed: make(chan struct{})}
	go s.serve()
	return s, nil
}

// Addr 返回实际监听地址。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close 停止监听。
func (s *Server) Close() error {
	var err error
	s.once.Do(func() {
		close(s.closed)
		err = s.ln.Close()
	})
	return err
}

func (s *Server) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))

	// --- 握手 ---
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil {
		return
	}
	if hdr[0] != verSOCKS5 {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	// 仅支持无认证
	if _, err := c.Write([]byte{verSOCKS5, 0x00}); err != nil {
		return
	}

	// --- 请求 ---
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil {
		return
	}
	if req[0] != verSOCKS5 {
		return
	}
	if req[1] != cmdConnect {
		writeReply(c, 0x07) // Command not supported
		return
	}
	host, err := readAddr(c, req[3])
	if err != nil {
		writeReply(c, 0x08) // Address type not supported
		return
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(pb)
	target := net.JoinHostPort(host, strconv.Itoa(int(port)))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	remote, err := s.dial(ctx, "tcp", target)
	if err != nil {
		s.log("warn", fmt.Sprintf("代理连接 %s 失败: %v", target, err))
		writeReply(c, 0x05) // Connection refused
		return
	}
	defer remote.Close()

	if err := writeReply(c, 0x00); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	s.log("debug", "代理 -> "+target)

	// --- 双向转发 ---
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, c); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, remote); done <- struct{}{} }()
	<-done
}

func writeReply(c net.Conn, code byte) error {
	// VER REP RSV ATYP BND.ADDR BND.PORT
	_, err := c.Write([]byte{verSOCKS5, code, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

func readAddr(r io.Reader, atyp byte) (string, error) {
	switch atyp {
	case atypIPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case atypIPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return net.IP(b).String(), nil
	case atypDomain:
		lb := make([]byte, 1)
		if _, err := io.ReadFull(r, lb); err != nil {
			return "", err
		}
		b := make([]byte, int(lb[0]))
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return "", errors.New("unsupported address type")
	}
}
