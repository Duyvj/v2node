# Triển khai nhiều VPS

## Quy trình khuyến nghị

1. Cài bản gốc `wyx2685/v2node` và xác nhận `v2node status` hoạt động.
2. Áp RAM-fix trên một máy canary cho mỗi kiến trúc CPU.
3. Kiểm tra `v2node status`, `v2node version` và journal.
4. Rollout một batch nhỏ, theo dõi RAM, restart count và report traffic.
5. Chỉ triển khai toàn fleet khi batch nhỏ ổn định.

Luôn pin tag, URL và SHA-256 cho overlay. Không dùng release `latest`. Installer
`v0.4.4-ram4` đã pin sẵn asset + hash theo kiến trúc nên cùng một lệnh có thể dùng
cho cả fleet `amd64` và `arm64`.

## Lệnh cài pin theo tag

```bash
wget -O /root/v2node-install.sh \
  https://raw.githubusercontent.com/Duyvj/v2node/v0.4.4-ram4/script/install.sh
chmod 700 /root/v2node-install.sh
sudo /root/v2node-install.sh
```

Overlay từ chối chạy nếu VPS chưa có đầy đủ binary, config, menu và service của bản gốc.

## Hash release v0.4.4-ram4

```text
amd64  783183398c053c41571881ca3c22bbbdd40ad59628312cb9d16056bdc9a8af9a
arm64  2b9705727595ce2f08790a3b1411dcce55840005b75340a92a41afb26cf7a8d7
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
