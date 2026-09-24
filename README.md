# v2node optimized

Bản dựng lại từ [wyx2685/v2node](https://github.com/wyx2685/v2node) với các tối ưu
tham khảo [Mtoly/XrayRP](https://github.com/Mtoly/XrayRP): trạng thái limiter theo
user, giảm cấp phát trên đường kết nối, counter báo lưu lượng an toàn hơn, HTTP
có giới hạn bộ nhớ, tác vụ nền có vòng đời rõ ràng và reload có rollback.

Đọc [chi tiết tối ưu và giới hạn](docs/OPTIMIZATION.md) trước khi chuyển từ cây
Duyvj cũ. Bản này dùng cấu hình Nodes của upstream; không có Agent, terminal hay
Redis limiter của bản cũ. Giới hạn thiết bị là IP nguồn theo UUID và chưa là
giới hạn nguyên tử trên toàn cụm.

## Benchmark admission

Cùng máy Windows amd64, Go 1.26.8 jsonv2, CPU Xeon Gold 6240, 4 worker, 5.000 user,
3 lượt x 1 giây mỗi tình huống. Các lượt upstream và optimized chạy lần lượt,
không chạy đồng thời với test/build. Số dưới đây là trung vị:

| User hoạt động | Upstream ns/op | Optimized ns/op | Upstream B/op | Optimized B/op |
|---|---:|---:|---:|---:|
| 50 | 257.1 | 122.8 | 304 | 0 |
| 1.000 | 312.7 | 121.6 | 315 | 0 |
| 5.000 | 350.8 | 127.8 | 319 | 0 |

Cấp phát giảm từ 6–7 xuống 0 allocs/op. Đo riêng lượt kiểm tra đã có IP;
không phải benchmark băng thông VPN, CPU/RAM tổng, hay so sánh trực tiếp XrayRP.

## Cấu hình

- [config.example.json](config.example.json): giữ mặc định buffer/timeouts upstream.
- [config.low-memory.example.json](config.low-memory.example.json): mẫu buffer
  32 KiB, GOGC 80, Go soft memory limit 256 MiB; cần điều chỉnh theo VPS và tải.

Các giao thức/transport và DNS provider ACME có trong upstream được giữ nguyên.
Không thay core bằng XrayRP vì hai dự án có lớp tích hợp khác nhau.

## Cài đặt

Script trong repo này tải bản phát hành của **Duyvj/v2node**. Chỉ chạy sau khi có
release của bản optimized; chưa có release thì build từ source hoặc dùng artifact
đúng commit. Đọc mục chuyển đổi trong tài liệu; không dùng config Agent cũ.

```sh
curl -fsSLo install.sh https://raw.githubusercontent.com/Duyvj/v2node/main/script/install.sh
bash install.sh
```

## Build và kiểm tra

```sh
GOEXPERIMENT=jsonv2 go test -p 2 -race -count=1 -timeout=180s ./...
CGO_ENABLED=0 GOEXPERIMENT=jsonv2 go build -p 2 -trimpath -o v2node \
  -ldflags "-s -w -X github.com/wyx2685/v2node/cmd.version=optimized-local" .
```

Build yêu cầu Go 1.26. Workflow chạy test race trước khi build các nền tảng.
Bản phát hành vẫn dùng tên archive tương thích script upstream.

## Nguồn và giấy phép

- Upstream nền: `ad749f5220c800dc2dca65992d4b3f6864027855`.
- XrayRP tham khảo: `988fc5f5920050920b2f630a9cd8895256606d17`.
- [MPL-2.0](LICENSE), giữ giấy phép và attribution upstream.
