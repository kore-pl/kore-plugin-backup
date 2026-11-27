// Package main 是 kore 的 backup 插件：本地 + S3 兼容备份的方法面骨架。
//
// backup 只做协议握手 + 参数校验 + 结构化结果，不真写任何存储、
// 不内置 awscli/rclone 等任何工具二进制（裁决37 / 提案 §2.1 批2）。
//
// 协议面（kore.plugin.v1）：UDS → kore.hello 握手 → 会话循环读 request、
// 按 method 分发并回 response；stdout 留给协议，日志一律写 stderr。
package main

import (
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/pengtang1024/kore-plugin-backup/internal/backupop"
)

// main 按退出码区分失败：0 优雅退出、1 启动或协议失败。
func main() {
	logger := newLogger(os.Getenv("KORE_LOG_LEVEL"))
	if err := run(logger); err != nil {
		logger.Error("插件退出", "err", err)
		os.Exit(1)
	}
}

// run 依次完成建连、hello 握手与会话循环；收到 kore.shutdown 后返回 nil。
func run(logger *slog.Logger) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// 启动时清理上次进程遗留的半包（>30 分钟未提交），不碰任何已提交备份。
	if err := backupop.SweepStaleUploads(cfg.backupDir); err != nil {
		logger.Warn("清理过期上传暂存失败", "err", err)
	}
	conn, err := net.Dial("unix", cfg.socket)
	if err != nil {
		return fmt.Errorf("连接主壳 socket %s: %w", cfg.socket, err)
	}
	defer func() { _ = conn.Close() }()
	if err := handshake(conn, cfg); err != nil {
		return err
	}
	logger.Info("hello 完成", "plugin_id", cfg.id, "version", cfg.version)
	return serve(conn, cfg, logger)
}
