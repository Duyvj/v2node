# v2node-personal

Bản mod riêng từ [wyx2685/v2node](https://github.com/wyx2685/v2node) `v0.4.4`, tối ưu cho nhiều VPS nhỏ, đặc biệt máy 1 vCPU/1 GiB RAM. Bản mod giữ nguyên protocol, API v2board, quản lý user và số liệu traffic dùng để báo cáo về panel.

## Điểm đã mod

- Giảm Xray buffer mặc định từ 128 KiB xuống 64 KiB; có thể chỉnh bằng `Runtime.BufferSizeKB`.
- Bỏ bộ đếm traffic per-user trùng lặp của Xray nhưng vẫn giữ bộ đếm v2node để report panel.
- Chặn polling interval lỗi, tránh busy loop và panic khi panel thiếu `base_config`.
- Giảm allocation khi kiểm tra IP/device limit; sửa map/counter đồng thời và snapshot alive-list.
- Reset traffic bằng atomic swap để không làm mất byte đến đúng lúc report.
- Che token/API key khỏi lỗi REST và journal; khóa private key/ACME account ở mode `0600`.
- Sửa vòng đời timer task, file-descriptor leak và một số lỗi cạnh tranh dữ liệu.
- Installer xác minh SHA-256, kiến trúc ELF, dùng release versioned + symlink atomic, health gate, backup và rollback giao dịch.
- Profile systemd theo RAM/cgroup với `GOMEMLIMIT`, `MemoryHigh`, `TasksMax`, `LimitNOFILE`, `LimitCORE=0` và log rate limit.

Chi tiết kỹ thuật nằm trong [MODIFICATIONS.md](MODIFICATIONS.md). Baseline upstream được ghi tại [release/UPSTREAM.md](release/UPSTREAM.md).

## Cài trực tiếp từ GitHub Release

Ví dụ cho VPS `amd64/x86_64`:

```bash
curl --fail --location --proto '=https' --proto-redir '=https' \
  -o /root/v2node-install.sh \
  https://raw.githubusercontent.com/Duyyvj/v2node/v0.4.4-p1/deploy/install.sh
chmod 700 /root/v2node-install.sh
sudo /root/v2node-install.sh \
  --package-url https://github.com/Duyyvj/v2node/releases/download/v0.4.4-p1/v2node-personal-v0.4.4-p1-linux-64.zip \
  --sha256 ec72782211e683c1804bb4f56f1cb4240a4e29236491b9c722beb9276299dd2f \
  --config-file /root/v2node-config.json
```

VPS `arm64/aarch64` dùng:

- package: `v2node-personal-v0.4.4-p1-linux-arm64-v8a.zip`
- SHA-256: `76d373985fe8695d2612b1de2878d510396b46960ebbf313d4f1eda28f76d490`

Nếu `/etc/v2node/config.json` đã tồn tại và đúng, có thể bỏ `--config-file`. Không đặt API key trực tiếp trên command line; dùng config mode `0600`, `--api-key-file` hoặc `--api-key-stdin`.

> Nếu repository để **private**, các URL raw/release trên VPS cần GitHub token. Với fleet nhiều VPS, nên dùng repo/release public hoặc mirror artifact sang HTTPS storage riêng có cơ chế xác thực phù hợp.

## Quản lý và rollback

```bash
v2nodectl status
v2nodectl log
v2nodectl restart
v2nodectl version
v2nodectl rollback
```

Installer giữ tối thiểu ba release, kiểm tra service ổn định trước khi commit và tự khôi phục service/config/sysctl/swap/symlink nếu giao dịch lỗi. `MemoryHigh` là ngưỡng mềm; bản mod không đặt hard `MemoryMax` có thể chủ động OOM-kill service.

## Build

Yêu cầu Go `1.26.1`, `GOEXPERIMENT=jsonv2`, Bash và `zip`:

```bash
bash build/build.sh
cat artifacts/SHA256SUMS
```

Build script chạy `go mod verify`, `go test ./...`, `go vet ./...`, sau đó cross-build Linux amd64 và arm64 với `CGO_ENABLED=0`, `-trimpath`, `-s -w`.

## Triển khai nhiều VPS

Rollout theo thứ tự: một canary cho mỗi kiến trúc, một nhóm nhỏ, rồi mới chạy toàn fleet. Luôn pin tag, URL và SHA-256; không dùng URL `latest`. Xem [docs/FLEET_DEPLOYMENT.md](docs/FLEET_DEPLOYMENT.md).

## Tương thích

- Artifact: Linux amd64 và arm64, 64-bit little-endian.
- Host: systemd cùng bộ công cụ GNU thông dụng; thiết kế cho Debian/Ubuntu.
- Protocol, sniffing, routing và timeout handshake giữ hành vi upstream.
- Nếu ưu tiên throughput hơn RAM, đặt `Runtime.BufferSizeKB` về `128`.
