# Tối ưu từ nền v2node gốc

## Nguồn và phạm vi

- Nền: [wyx2685/v2node, ad749f5](https://github.com/wyx2685/v2node/tree/ad749f5220c800dc2dca65992d4b3f6864027855).
- Tham khảo: [Mtoly/XrayRP, 988fc5f](https://github.com/Mtoly/XrayRP/tree/988fc5f5920050920b2f630a9cd8895256606d17).
- XrayRP: `common/limiter/limiter.go` và `limiter_benchmark_test.go` (trạng thái theo user,
  TTL, workload 5.000 user); `service/controller/reporting.go` (giữ counter khi báo lỗi);
  `api/internal/panelhttp/response_limit.go` (giới hạn body);
  `cmd/config_reload.go` (quản lý vòng đời và rollback).

Đây là bản dựng lại từ upstream, giữ core wyx2685/xray-core đã pin trong go.mod
để giữ giao thức và API của v2node. Các cơ chế trên được triển khai thích ứng;
không có số đo nào trong tài liệu này là benchmark trực tiếp với XrayRP.

## Thay đổi

1. Limiter dùng khóa riêng từng user và map IP có TTL. IP hiện có không cấp phát
   lại sync.Map mỗi handshake. Bucket tốc độ được dùng lại; giới hạn thay đổi
   áp dụng vào bucket của kết nối đang mở. Chuẩn hóa IPv4-mapped IPv6.
2. Kiểm tra TCP/UDP thống nhất, limit cục bộ được kiểm tra nguyên tử. Báo online
   không xóa trạng thái admission. Tối đa 256 IP được theo dõi mỗi user để chặn
   tăng RAM vô hạn; user không giới hạn vẫn được truyền nếu vượt trần theo dõi.
3. Traffic counter và link manager khởi tạo nguyên tử. Buffer bị từ chối không
   được tính lưu lượng; reader giữ payload cuối cùng dù kèm EOF và tôn trọng timeout.
4. Snapshot lưu lượng giữ danh tính counter, chỉ trừ đúng số byte khi HTTP báo
   thành công. Byte phát sinh trong lúc chờ phản hồi được giữ lại. POST báo lưu
   lượng không tự retry trong HTTP client. Panel legacy chưa có idempotency nên
   mất phản hồi sau khi panel ghi nhận vẫn có thể dẫn đến tính lại ở kỳ sau.
5. Tác vụ nền có một worker, retry lỗi tạm thời, hủy và chờ dừng trước đóng node.
   Timeout không sinh thêm callback chạy chồng. Close báo lỗi nếu callback không
   phản hồi cancellation trong 5 giây. ACME upstream chưa hỗ trợ hủy mọi thao tác.
6. Watcher theo dõi thư mục, hỗ trợ thay config bằng rename, gom burst 500 ms,
   không sửa cấu hình đang chạy từ goroutine watcher. Reload đọc config/panel
   trước khi đóng listeners; lỗi khởi động candidate thử khôi phục snapshot trước.
7. HTTP pool có giới hạn; body panel tối đa 16 MiB, user tối đa 100.000.
   Msgpack kiểm tra chiều dài array trước cấp phát. ETag chỉ cập nhật sau parse
   thành công. Danh sách user rỗng được áp dụng; lỗi API không trở thành snapshot rỗng.
8. Buffer, timeout, GOGC và memory limit cấu hình được. Mặc định giữ upstream.
   Metadata-only sniffing là tùy chọn và bỏ cấp phát buffer nội dung khi bật.

## Cấu hình

Dùng `config.example.json` cho cấu hình tương thích mặc định. Ví dụ
`config.low-memory.example.json` đặt buffer 32 KiB, GOGC=80 và Go soft memory
limit=256 MiB. Đây là điểm khởi đầu để đo trên VPS nhỏ, không phải cam kết tổng
RSS luôn dưới 256 MiB. Buffer nhỏ/GOGC thấp có thể tăng CPU hoặc giảm throughput.
Giá trị Resource bằng 0 giữ thiết lập từ môi trường Go/default của tiến trình.

MetadataOnlySniffing=false giữ dò nội dung như upstream. Bật true chỉ khi không
cần routing dựa trên TLS SNI/HTTP/QUIC payload; metadata và FakeDNS vẫn được dùng.
Không gọi FreeOSMemory định kỳ hoặc ép GC mỗi request.

Để cập nhật các thay đổi user/limit trên phiên đang mở, luồng có user đi qua
buffer wrappers; splice được tắt ở các phiên đó. Cần đo băng thông và CPU trên
traffic thực tế trước rollout diện rộng, đặc biệt VLESS Vision.

## Giới hạn thiết bị

Thiết bị vẫn được ước lượng bằng IP nguồn cho từng UUID. Hai máy chung NAT tính
một IP; IPv4 và IPv6 khác nhau tính hai. TTL IP là 60 giây và được refresh khi
có traffic. Khi giảm limit, giữ các IP dùng gần nhất và chặn phần thừa lúc truyền.

Giới hạn cục bộ được bảo đảm dưới khóa riêng user. Số `/alivelist` của panel là
gợi ý cho admission IP mới, có độ trễ và có thể chứa IP đang reconnect. Nó
không phải giao dịch nguyên tử giữa các node. Bản nền mới này không có Redis
limiter của bản Duyvj cũ; không thể cam kết tổng toàn cụm luôn là 2 IP. Chính
XrayRP mô tả global device cache là hỗ trợ có độ trễ/fail-open, không phải strict
distributed limiter. Muốn chặn toàn cụm cần một bước tích hợp admission dùng chung.

## Chuyển từ bản Duyvj cũ

Bản dựng lại này không mang theo Agent/terminal, Redis snapshot, Redis limiter,
sticky balancer và traffic spool của cây source trước. Dùng cấu hình Nodes của
upstream. Cấu hình Agent cần chuyển trước khi dùng bản mới. Lịch sử Git giữ bản
cũ để có thể quay lại. Source mới chưa được cài trên VPS sản xuất.

Counters và backlog hiện nằm trong RAM; không cam kết giữ số byte chưa báo qua
crash/restart. Các giao thức/transport và danh sách DNS provider ACME của upstream
được giữ, nên kích thước binary vẫn gồm các provider đó.

## Kiểm tra

```sh
GOEXPERIMENT=jsonv2 go test -p 2 -race -count=1 -timeout=180s ./...
GOEXPERIMENT=jsonv2 go test ./limiter -run '^$' -bench BenchmarkCheckLimit -benchmem -count=3 -cpu=4
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOEXPERIMENT=jsonv2 go build -p 2 -trimpath -o v2node .
```

Test dùng httptest/loopback, không gọi panel hay Redis sản xuất. Benchmark so sánh
cùng harness với upstream; chỉ đo admission đã có IP, không đo băng thông VPN,
P99 latency mạng, RSS trên VPS hay khả năng chứa người dùng thực tế.
