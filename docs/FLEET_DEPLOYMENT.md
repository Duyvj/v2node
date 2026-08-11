# Triển khai nhiều VPS

## Quy trình khuyến nghị

1. Cài bản gốc `wyx2685/v2node` và xác nhận `v2node status` hoạt động.
2. Áp RAM-fix trên một máy canary cho mỗi kiến trúc CPU.
3. Kiểm tra `v2node status`, `v2node version` và journal.
4. Rollout một batch nhỏ, theo dõi RAM, restart count và report traffic.
5. Chỉ triển khai toàn fleet khi batch nhỏ ổn định.

Luôn pin tag, URL và SHA-256 cho overlay. Không dùng release `latest`. Installer
`v0.4.4-ram5` đã pin sẵn asset + hash theo kiến trúc nên cùng một lệnh có thể dùng
cho cả fleet `amd64` và `arm64`.

## Lệnh cài pin theo tag

```bash
wget -O /root/v2node-install.sh \
  https://raw.githubusercontent.com/Duyvj/v2node/v0.4.4-ram5/script/install.sh
chmod 700 /root/v2node-install.sh
sudo /root/v2node-install.sh
```

Overlay từ chối chạy nếu VPS chưa có đầy đủ binary, config, menu và service của bản gốc.

## Hash release v0.4.4-ram5

```text
amd64  db1d0e83fbfff5b7b243fa0e9f469230964b083cf3339a68d6a48ec58f93e038
arm64  ad88e6d888318e4875a1e28b37fa360ba5605ef7cc4f44ba866e1b4c8f54c2fd
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
