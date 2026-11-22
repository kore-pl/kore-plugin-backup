# Changelog

## 0.1.0 (2026-11-22)

- feat: 新建 backup 插件骨架，复用 kore.plugin.v1 握手与会话循环
- feat: backup.jobs / backup.create 方法级参数校验与结构化结果，目标路径白名单 + retention 天数校验，不真写本地或 S3 存储
- feat: 四类故障注入测试（kill -9 / 超时 / 磁盘满 / cgroup 超限），验证方法级错误返回
- build: 确定性打包（--sort=name --mtime=@0 --owner=0 --numeric-owner + chmod 归一 + gzip -9n）
