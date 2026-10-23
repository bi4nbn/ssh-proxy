package sshx

import "io"

// copyTo 只是 io.Copy 的薄封装，便于在转发协程里统一处理错误。
func copyTo(dst io.Writer, src io.Reader) (int64, error) { return io.Copy(dst, src) }
