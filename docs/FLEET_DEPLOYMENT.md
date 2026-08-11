# Triển khai nhiều VPS

## Quy trình khuyến nghị

1. Cài bản gốc `wyx2685/v2node` và xác nhận `v2node status` hoạt động.
2. Áp RAM-fix trên một máy canary cho mỗi kiến trúc CPU.
3. Kiểm tra `v2node status`, `v2node version` và journal.
4. Rollout một batch nhỏ, theo dõi RAM, restart count và report traffic.
5. Chỉ triển khai toàn fleet khi batch nhỏ ổn định.

Luôn pin tag, URL và SHA-256 cho overlay. Không dùng release `latest`. Installer
`v0.4.4-ram3` đã pin sẵn asset + hash theo kiến trúc nên cùng một lệnh có thể dùng
cho cả fleet `amd64` và `arm64`.

## Lệnh cài pin theo tag

```bash
wget -O /root/v2node-install.sh \
  https://raw.githubusercontent.com/Duyvj/v2node/v0.4.4-ram3/script/install.sh
chmod 700 /root/v2node-install.sh
sudo /root/v2node-install.sh
```

Overlay từ chối chạy nếu VPS chưa có đầy đủ binary, config, menu và service của bản gốc.

## Hash release v0.4.4-ram3

```text
amd64  47cad5ad7737a2872d4b5e54f885e79cc8f750a454f259bb8a9235c6d3c79c58
arm64  3d8894cf5a2306522168959f6448a4f3d83735d386363742b256b36d2332370c
```

Trong canary, theo dõi ít nhất RSS/cgroup memory, số goroutine, connection hiện
hoạt, `NRestarts` của systemd và việc report traffic/user về panel. RAM có thể tăng
theo tải đang hoạt động nhưng không được tiếp tục giữ đỉnh cũ sau churn/reload.

## Rollback

```bash
sudo /usr/local/lib/v2node-ramfix/install.sh --rollback
v2node status
```

Installer cũng tự rollback khi package, ELF, service start hoặc health gate không đạt yêu cầu.
