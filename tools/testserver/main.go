// 本地测试用 SSH 服务端（不属于发布产物）。
// 支持密码/公钥认证、direct-tcpip 转发、keepalive 应答，用于验证代理链路。
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"

	"golang.org/x/crypto/ssh"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:2222", "监听地址")
	user := flag.String("user", "test", "用户名")
	pass := flag.String("pass", "test123", "密码")
	flag.Parse()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		log.Fatal(err)
	}

	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			if c.User() == *user && string(p) == *pass {
				return nil, nil
			}
			return nil, fmt.Errorf("密码错误")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if c.User() == *user {
				return nil, nil
			}
			return nil, fmt.Errorf("用户不存在")
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("测试 SSH 服务已启动: %s (用户 %s / 密码 %s)", *addr, *user, *pass)
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go handle(c, cfg)
	}
}

func handle(nc net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	log.Printf("客户端接入: %s (%s)", sc.RemoteAddr(), sc.ClientVersion())

	go func() {
		for req := range reqs {
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		}
	}()

	for nch := range chans {
		switch nch.ChannelType() {
		case "direct-tcpip":
			go handleDirect(nch)
		default:
			_ = nch.Reject(ssh.UnknownChannelType, "不支持的通道类型")
		}
	}
}

func handleDirect(nch ssh.NewChannel) {
	// string host, uint32 port, string origHost, uint32 origPort
	b := nch.ExtraData()
	if len(b) < 8 {
		_ = nch.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	hl := binary.BigEndian.Uint32(b[:4])
	host := string(b[4 : 4+hl])
	idx := 4 + int(hl)
	port := binary.BigEndian.Uint32(b[idx : idx+4])
	target := net.JoinHostPort(host, fmt.Sprint(port))

	remote, err := net.Dial("tcp", target)
	if err != nil {
		_ = nch.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := nch.Accept()
	if err != nil {
		remote.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	log.Printf("转发 -> %s", target)
	go func() { _, _ = io.Copy(remote, ch); remote.Close() }()
	_, _ = io.Copy(ch, remote)
	_ = ch.Close()
}
