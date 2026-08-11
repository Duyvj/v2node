# Triển khai nhiều VPS

## Quy trình khuyến nghị

1. Cài trực tiếp nhánh standalone trên một máy canary cho mỗi kiến trúc CPU.
2. Nhập config panel hoặc truyền đủ `--api-host`, `--node-id` và `--api-key`.
3. Kiểm tra `v2node status`, `v2node version`, journal và report traffic.
4. Rollout một batch nhỏ; theo dõi RAM, restart count và băng thông thực tế.
5. Chỉ triển khai toàn fleet khi batch nhỏ ổn định.

Không cần cài `wyx2685/v2node` trước. Installer tự tạo binary, geodata, config,
menu `v2node`, service và profile RAM ram5.

## Lệnh cài standalone

```bash
(
  set -e
  installer="$(mktemp /tmp/v2node-install.XXXXXX)"
  trap 'rm -f -- "$installer"' EXIT
  curl -fL https://raw.githubusercontent.com/Duyvj/v2node/upgraded-v0.4.4/script/install.sh \
    -o "$installer"
  chmod 700 "$installer"
  sudo "$installer"
)
```

Triển khai không tương tác:

```bash
(
  set -e
  installer="$(mktemp /tmp/v2node-install.XXXXXX)"
  trap 'rm -f -- "$installer"' EXIT
  curl -fL https://raw.githubusercontent.com/Duyvj/v2node/upgraded-v0.4.4/script/install.sh \
    -o "$installer"
  chmod 700 "$installer"
  sudo "$installer" \
    --api-host https://panel.example/ \
    --node-id 1 \
    --api-key 'YOUR_API_KEY'
)
```

Khi tạo bản triển khai cố định cho production, nên pin URL về đúng commit hoặc tag
standalone đã kiểm tra. Binary bên trong vẫn luôn được lấy từ release bất biến
`v0.4.4-ram5` và xác minh bằng SHA-256; installer không dùng release `latest`.

## Hash release v0.4.4-ram5

```text
amd64  db1d0e83fbfff5b7b243fa0e9f469230964b083cf3339a68d6a48ec58f93e038
arm64  ad88e6d888318e4875a1e28b37fa360ba5605ef7cc4f44ba866e1b4c8f54c2fd
```

Trong canary, theo dõi ít nhất RSS/cgroup memory, số goroutine, connection hiện
hoạt, `NRestarts` của systemd và việc report traffic/user về panel. RAM có thể tăng
theo tải đang hoạt động nhưng không được tiếp tục giữ đỉnh cũ sau churn/reload.

## Nâng cấp và khôi phục lỗi

Chạy lại cùng installer để nâng cấp; `/etc/v2node/config.json` hiện có được giữ
nguyên. Tất cả asset được tải và kiểm tra trước khi service dừng. Nếu việc thay file,
áp profile hoặc health check thất bại, installer tự khôi phục file và trạng thái
active/enabled có trước lần chạy đó.

Luôn giữ máy canary và bản commit/tag đã chạy ổn định để có thể quay lại phiên bản
đã biết tốt trước khi rollout tiếp.
