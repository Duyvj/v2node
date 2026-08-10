# v2node-personal v0.4.4-ram2

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
wget -O /root/v2node-personal-ram2.sh \
  https://raw.githubusercontent.com/Duyvj/v2node/v0.4.4-ram2/script/install.sh
chmod 700 /root/v2node-personal-ram2.sh
sudo /root/v2node-personal-ram2.sh
```

Installer giữ nguyên từng byte và metadata của config đang dùng, lưu service/binary/
geodata/menu cũ vào `/var/backups/v2node`, dừng service cũ trước khi đổi file live,
cài release mới qua symlink nguyên tử rồi health-check trước khi commit. Nếu service
mới không ổn định, toàn bộ layout bản gốc được khôi phục tự động. Các đường dẫn cũ
`/usr/local/v2node/v2node`, `geoip.dat`,
`geosite.dat` và lệnh `v2node` vẫn hoạt động nên panel/VPN không phải đổi cấu hình.

VPS mới có thể truyền file config root-only:

```bash
chmod 600 /root/v2node-config.json
sudo /root/v2node-personal-ram2.sh --config-file /root/v2node-config.json
```

Installer tự nhận `amd64/x86_64` hoặc `arm64/aarch64`, dùng đúng asset của tag
`v0.4.4-ram2` và SHA-256 được nhúng sẵn; không truy vấn release `latest`.

Checksum chính thức nằm trong `release/SHA256SUMS` và asset `SHA256SUMS` của
GitHub Release. Installer đã pin cùng các giá trị đó theo từng kiến trúc.

Không đặt API key trên command line. Dùng config mode `0600`, `--api-key-file`
hoặc `--api-key-stdin`. Nếu repository để private, raw URL và release asset cần
cơ chế xác thực riêng; không nhúng GitHub token dài hạn vào script fleet.

## Quản lý và rollback

```bash
v2nodectl status
v2nodectl log
v2nodectl restart
v2nodectl version
v2nodectl rollback
v2node
```

Installer kiểm tra SHA-256, cấu trúc ZIP, kiến trúc ELF, health gate và thực hiện
rollback giao dịch nếu cài đặt lỗi. Backup được kiểm tra checksum trước khi restore;
`v2nodectl rollback` cũng khôi phục menu, binary, geodata của bản gốc và snapshot
`/etc/fstab` nguyên bản nếu giao dịch đã tạo swap. Theo RAM/cgroup hiệu dụng, systemd
được cấu hình `GOMEMLIMIT` khoảng 45%, `MemoryHigh` 65%, trần cứng `MemoryMax` 80%
và `MemorySwapMax` 10% (tối thiểu 128 MiB, tối đa 512 MiB), cùng log rate limit và
emergency swap.
RAM thực tế vẫn tăng hợp lý theo số connection đang hoạt động;
nếu chạm trần cứng, systemd khởi động lại service thay vì để cả VPS cạn RAM.

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

Yêu cầu Go `1.26.1`, `GOEXPERIMENT=jsonv2`, Bash và `zip`:

```bash
bash build/build.sh
cat artifacts/SHA256SUMS
```

Build script chạy verify/test/vet cho v2node và toàn bộ package Xray đã vá, sau
đó cross-build Linux `amd64`/`arm64` với `CGO_ENABLED=0`, `-trimpath`, `-s -w`.

Triển khai fleet theo thứ tự canary → batch nhỏ → toàn bộ. Xem
[docs/FLEET_DEPLOYMENT.md](docs/FLEET_DEPLOYMENT.md).
