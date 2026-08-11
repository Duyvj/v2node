# v2node v0.4.4-ram5 — bản standalone ổn định RAM

Đây là nhánh đầy đủ của [wyx2685/v2node](https://github.com/wyx2685/v2node)
`v0.4.4` đã tích hợp sẵn các bản sửa RAM. Có thể cài trực tiếp lên VPS trắng,
không cần cài v2node gốc trước và vẫn quản lý bằng đúng lệnh `v2node`.

Protocol, routing, sniffing, panel API, quản lý user và report traffic giữ nguyên
baseline upstream. Nhánh này chỉ thay các điểm gây giữ RAM lâu dài, bổ sung giới
hạn an toàn và làm lại quy trình cài/cập nhật.

## Cài trực tiếp trên VPS trắng

Đăng nhập bằng `root` rồi chạy một lệnh:

```bash
(
  set -e
  installer="$(mktemp /tmp/v2node-install.XXXXXX)"
  trap 'rm -f -- "$installer"' EXIT
  curl -fL https://raw.githubusercontent.com/Duyvj/v2node/upgraded-v0.4.4/script/install.sh \
    -o "$installer"
  chmod 700 "$installer"
  bash "$installer"
)
```

Installer sẽ hỏi thông tin panel nếu terminal đang ở chế độ tương tác. Có thể cài
không cần hỏi bằng cách truyền đủ ba tham số:

```bash
(
  set -e
  installer="$(mktemp /tmp/v2node-install.XXXXXX)"
  trap 'rm -f -- "$installer"' EXIT
  curl -fL https://raw.githubusercontent.com/Duyvj/v2node/upgraded-v0.4.4/script/install.sh \
    -o "$installer"
  chmod 700 "$installer"
  bash "$installer" \
    --api-host https://panel.example/ \
    --node-id 1 \
    --api-key 'YOUR_API_KEY'
)
```

Nếu không nhập thông tin panel, toàn bộ chương trình vẫn được cài nhưng service
được để dừng và chưa bật khởi động cùng hệ thống, tránh vòng lặp lỗi với config mẫu.
Sau đó chạy:

```bash
v2node generate
```

Các lệnh quản lý vẫn giống bản gốc:

```bash
v2node
v2node status
v2node log
v2node restart
v2node version
v2node update
```

## Cài đè hoặc nâng cấp

Có thể chạy lại đúng installer trên máy đã có v2node gốc, ram3, ram4 hoặc ram5.
File `/etc/v2node/config.json` hiện có được giữ nguyên nội dung. Installer tải và
xác minh toàn bộ binary, geodata, config template và menu trước khi dừng service;
nếu thay file hoặc health check thất bại, trạng thái trước đó được khôi phục trong
cùng lần chạy.

Menu `v2node install`, `v2node update` và `v2node update_shell` đều được khóa về
nhánh `upgraded-v0.4.4` này, nên không vô tình cài lại binary upstream chưa sửa.

Installer chỉ chấp nhận Linux `amd64/x86_64` hoặc `arm64/aarch64` và pin đúng
binary của release `v0.4.4-ram5`:

| Kiến trúc | SHA-256 archive |
|---|---|
| amd64 | `db1d0e83fbfff5b7b243fa0e9f469230964b083cf3339a68d6a48ec58f93e038` |
| arm64 | `ad88e6d888318e4875a1e28b37fa360ba5605ef7cc4f44ba866e1b4c8f54c2fd` |

Geodata và config template cũng được tải từ tag bất biến và kiểm tra SHA-256.
Không truy vấn `latest`, không đoán amd64 khi gặp CPU lạ và không tải installer
upstream trong nền.

## Vì sao bản gốc có thể tăng RAM theo thời gian

Không chỉ có Go heap: bản gốc và Xray fork giữ nhiều state theo connection/IP,
session UDP, fragment, packet chờ, cache phòng thủ, request body, timer và
goroutine cleanup. Một số cấu trúc không có giới hạn tổng hoặc không trả ownership
khi close/timeout, nên tải bất thường và churn lâu ngày có thể đẩy live memory lên
liên tục. Chỉ đặt `GOMEMLIMIT` không sửa được dữ liệu vẫn còn được tham chiếu.

Bản này xử lý ở nguồn:

- giới hạn online-IP theo user/toàn node và thay map sau mỗi kỳ report;
- giới hạn cache/session/queue/packet/fragment trong Shadowsocks, AnyTLS,
  Hysteria, TUIC, UDP worker, XHTTP/splitHTTP và gRPC;
- đóng/join task, watcher, timer, stream, body, socket, transport và connection;
- reload bằng thay thế process sau graceful cleanup để thu hồi toàn bộ core cũ;
- giảm buffer mặc định từ 128 KiB xuống 64 KiB và bỏ Xray user-stat trùng lặp;
- parser panel có giới hạn byte/user và không coi response lỗi là danh sách rỗng;
- sửa race khi thêm/xóa user, traffic counter và link manager;
- lỗi startup/reload trả exit code khác 0 để service manager tự khởi động lại.

Chi tiết kỹ thuật nằm trong [MODIFICATIONS.md](MODIFICATIONS.md) và
[Xray RAM patch](third_party/xray-core/V2NODE_RAM_PATCH.md). Baseline chính xác
được ghi tại [release/UPSTREAM.md](release/UPSTREAM.md).

## Profile RAM ram5

Trên systemd, installer tạo drop-in
`/etc/systemd/system/v2node.service.d/90-v2node-ramfix.conf`. Profile ưu tiên tải
đồng thời cao: chừa cho hệ điều hành `max(384 MiB, 15% RAM)` nhưng không quá 25%
RAM trên VPS nhỏ; `MemoryMax` dùng phần còn lại. `GOMEMLIMIT` chừa
`max(256 MiB, 10% trần service)` và `MemoryHigh` chỉ là ngưỡng áp lực gần trần
cứng. `MemorySwapMax` là 10% RAM, clamp 128–512 MiB.

| RAM/cgroup hiệu dụng | Chừa cho host | GOMEMLIMIT | MemoryHigh | MemoryMax |
|---:|---:|---:|---:|---:|
| 2 GiB | 384 MiB | 1408 MiB | 1536 MiB | 1664 MiB |
| 4 GiB | 614 MiB | 3134 MiB | 3308 MiB | 3482 MiB |

Đây không phải một giới hạn RAM thấp cố định. VPS lớn tự có thêm headroom cho nhiều
người dùng; trần cứng chỉ là lớp bảo vệ cuối để riêng service được restart thay vì
làm cả VPS cạn RAM. Nếu config có `Runtime.MemoryLimit` không rỗng, giá trị đó sẽ
chủ động thay `GOMEMLIMIT` tự động.

Alpine/OpenRC vẫn dùng đúng binary và `GOMEMLIMIT` ram5, nhưng OpenRC không cung
cấp `MemoryHigh`/`MemoryMax`/`MemorySwapMax` như systemd. Với systemd cũ, installer
dùng `MemoryLimit` tương thích và báo rõ directive nào không được hệ thống hỗ trợ.

Sau khi tăng hoặc giảm RAM VPS, chạy lại installer để tính lại profile.

## Runtime guardrails

```json
{
  "Runtime": {
    "MinPollIntervalSeconds": 30,
    "MaxPollIntervalSeconds": 3600,
    "BufferSizeKB": 64,
    "MaxTrackedIPsPerUser": 256,
    "MaxTrackedIPsPerNode": 32768,
    "MaxPanelResponseBytes": 16777216,
    "MaxUsers": 100000
  }
}
```

Nếu ưu tiên throughput hơn RAM, có thể đặt `Runtime.BufferSizeKB` về `128`.

## Build và kiểm thử

Yêu cầu Go `1.26.1`, `GOEXPERIMENT=jsonv2` và Bash:

```bash
bash build/build.sh
cat artifacts/SHA256SUMS
```

Build script chạy verify/test/vet cho v2node và các package Xray đã vá, sau đó
cross-build Linux amd64/arm64 với `CGO_ENABLED=0`, `-trimpath` và `-s -w`.

Triển khai nhiều VPS theo thứ tự canary → batch nhỏ → toàn bộ. Xem
[docs/FLEET_DEPLOYMENT.md](docs/FLEET_DEPLOYMENT.md).
