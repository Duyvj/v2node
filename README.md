# v2node v0.4.4-ram5 — RAM fix overlay

Bản mod RAM-only từ [wyx2685/v2node](https://github.com/wyx2685/v2node)
`v0.4.4`, dành cho nhiều VPS nhỏ và máy chạy lâu ngày. Protocol, routing,
sniffing, panel API, quản lý user và report traffic giữ nguyên baseline upstream.

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
- lỗi startup/reload trả exit code khác 0 để systemd tự khởi động lại.

Chi tiết kỹ thuật: [MODIFICATIONS.md](MODIFICATIONS.md) và
[Xray RAM patch](third_party/xray-core/V2NODE_RAM_PATCH.md). Baseline chính xác
được ghi tại [release/UPSTREAM.md](release/UPSTREAM.md).

## Cài đè an toàn lên bản gốc

Nếu VPS đã cài bản gốc bằng `wyx2685/v2node` và có
`/etc/v2node/config.json` hợp lệ, chỉ cần chạy:

```bash
wget -O /root/v2node-ramfix.sh \
  https://raw.githubusercontent.com/Duyvj/v2node/v0.4.4-ram5/script/install.sh
chmod 700 /root/v2node-ramfix.sh
sudo /root/v2node-ramfix.sh
```

Đây chỉ là overlay, không phải bộ cài v2node mới. Installer yêu cầu bản gốc đã có
binary, config, menu và `v2node.service`; nó chỉ thay nguyên tử
`/usr/local/v2node/v2node` bằng binary RAM fix và thêm drop-in
`/etc/systemd/system/v2node.service.d/90-v2node-ramfix.conf`.

`/etc/v2node/config.json`, `/usr/bin/v2node`, file `v2node.service` chính và toàn bộ
geodata được giữ nguyên từng byte lẫn metadata. Trước khi thay binary, installer lưu
binary/drop-in cũ tại `/var/backups/v2node-ramfix`, ghi checksum của mọi file phải giữ
nguyên, tự kiểm tra backup rồi mới dừng service. Nếu health check không đạt, binary
gốc được khôi phục tự động.

Nếu máy từng cài `ram1/ram2` và binary đang là symlink, installer dừng trước mọi
thay đổi. Hãy cài lại upstream `v0.4.4` rồi mới chạy overlay này; ram5 không tự
rollback layout cũ để tránh ghi đè config đã được chỉnh sau đó. Máy đang chạy
`ram3/ram4` có thể cài đè trực tiếp lên `ram5`.

Installer tự nhận `amd64/x86_64` hoặc `arm64/aarch64`, dùng đúng asset của tag
`v0.4.4-ram5` và SHA-256 được nhúng sẵn; không truy vấn release `latest`.

Checksum chính thức nằm trong `release/SHA256SUMS` và asset `SHA256SUMS` của
GitHub Release. Installer đã pin cùng các giá trị đó theo từng kiến trúc.

## Quản lý và rollback

```bash
v2node
v2node status
v2node log
v2node restart
v2node version
```

Đây chính là menu/lệnh của bản gốc; overlay không cài thêm CLI nào. Rollback kỹ thuật
của overlay dùng installer đã xác minh:

```bash
sudo /usr/local/lib/v2node-ramfix/install.sh --rollback
```

Installer kiểm tra SHA-256, cấu trúc ZIP, kiến trúc ELF và health gate. Profile
`ram5` ưu tiên tải đồng thời cao: chừa cho hệ điều hành `max(384 MiB, 15% RAM)`
nhưng không quá 25% RAM trên VPS nhỏ; `MemoryMax` dùng phần còn lại. `GOMEMLIMIT`
chừa `max(256 MiB, 10% trần service)`, được cap ở một phần ba trần trên máy nhỏ.
`MemoryHigh` chừa `max(128 MiB, 5%)`, được cap ở một phần tư trần trên máy nhỏ.
`MemorySwapMax` vẫn là 10% RAM, clamp 128–512 MiB.

Giá trị danh nghĩa theo RAM/cgroup hiệu dụng:

| RAM | Chừa cho host | GOMEMLIMIT | MemoryHigh | MemoryMax |
|---:|---:|---:|---:|---:|
| 2 GiB | 384 MiB | 1408 MiB | 1536 MiB | 1664 MiB |
| 4 GiB | 614 MiB | 3134 MiB | 3308 MiB | 3482 MiB |

Vì `GOMEMLIMIT` và `MemoryHigh` cao hơn ram3, GC/reclaim không can thiệp sớm khi
nhiều người dùng đồng thời. Trần cứng vẫn là lớp bảo vệ cuối để riêng service được
restart thay vì làm cả VPS cạn RAM. Overlay không tạo swap hoặc sửa sysctl.
Nếu config đã đặt `Runtime.MemoryLimit`, giá trị đó sẽ chủ động thay `GOMEMLIMIT`
của drop-in. Sau khi nâng/hạ RAM VPS, hãy chạy lại installer để tính lại profile.

`ram5` được pin đúng baseline upstream `v0.4.4` và sẽ từ chối ghi đè một upstream
mới hơn. Nếu dùng `v2node update` lên phiên bản khác, chỉ áp lại overlay khi đã có
RAM-fix được build và kiểm thử cho đúng baseline đó.

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

Build script chạy verify/test/vet cho v2node và toàn bộ package Xray đã vá, sau
đó cross-build Linux `amd64`/`arm64` với `CGO_ENABLED=0`, `-trimpath`, `-s -w`.

Triển khai fleet theo thứ tự canary → batch nhỏ → toàn bộ. Xem
[docs/FLEET_DEPLOYMENT.md](docs/FLEET_DEPLOYMENT.md).
