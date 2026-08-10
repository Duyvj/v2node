# Triển khai nhiều VPS

## Quy trình khuyến nghị

1. Tạo `/root/v2node-config.json` mode `0600` trên từng VPS.
2. Cài một máy canary cho mỗi kiến trúc CPU.
3. Kiểm tra `v2nodectl status`, `v2nodectl version` và journal.
4. Rollout một batch nhỏ, theo dõi RAM, restart count và report traffic.
5. Chỉ triển khai toàn fleet khi batch nhỏ ổn định.

Luôn pin tag, URL và SHA-256. Không dùng release `latest`, không tải rồi chạy
installer upstream, và không đưa API key vào argv hoặc shell history. Installer
`v0.4.4-ram2` đã pin sẵn asset + hash theo kiến trúc nên cùng một lệnh có thể dùng
cho cả fleet `amd64` và `arm64`.

## Lệnh cài pin theo tag

```bash
wget -O /root/v2node-install.sh \
  https://raw.githubusercontent.com/Duyvj/v2node/v0.4.4-ram2/script/install.sh
chmod 700 /root/v2node-install.sh
sudo /root/v2node-install.sh
```

Máy mới chưa có `/etc/v2node/config.json` cần thêm `--config-file` hoặc bộ tham số
panel an toàn như mô tả trong `README.md`.

## Hash release v0.4.4-ram2

```text
amd64  d7090defddb5261842674dff04f408cec5d99351a667c95013cf6c68d8e41c34
arm64  34aeeada5dabbd9f2369cd0ab81995d966d1a2e8c2c63e84d4f95bd8544b90ab
```

Trong canary, theo dõi ít nhất RSS/cgroup memory, số goroutine, connection hiện
hoạt, `NRestarts` của systemd và việc report traffic/user về panel. RAM có thể tăng
theo tải đang hoạt động nhưng không được tiếp tục giữ đỉnh cũ sau churn/reload.

## Rollback

```bash
v2nodectl rollback
v2nodectl status
```

Installer cũng tự rollback khi package, ELF, service start hoặc health gate không đạt yêu cầu.
