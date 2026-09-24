# Giới hạn IP hoạt động trên nhiều node

## Phạm vi

Bản sửa dựa trên Duyvj/v2node commit `4f9a4bfc9188beab577c37c960bbba1cebd4f22b`.
Đối chiếu upstream wyx2685/v2node commit `ad749f5220c800dc2dca65992d4b3f6864027855`.
Upstream nhận tổng IP từ `/alivelist`, cho IP đã thấy tiếp tục dùng và không kiểm
tra nguyên tử giữa các node. Có nhánh bỏ qua kiểm tra nguồn UDP. Sao chép cơ chế
đó không bảo đảm tổng số IP hoạt động bằng device_limit.

Giới hạn trong bản này là IP nguồn chuẩn hóa theo từng UUID. Các máy chung IP
công cộng tính một slot; IPv4 và IPv6 khác nhau tính hai slot. Nhiều UUID của một
tài khoản có các bộ đếm riêng. Ràng buộc máy vật lý cần ứng dụng khách/panel cấp
và xác thực danh tính thiết bị; chỉ sửa node không thực hiện được việc này.

## Các thay đổi

- Redis tắt hoặc khởi tạo thất bại: truyền nil interface đúng, thực thi limit cục bộ.
- Kiểm tra cả lúc nhận kết nối và lúc truyền dữ liệu hai chiều, kể cả upload qua
  DispatchLink. Buffer bị từ chối không được chuyển tiếp hoặc tính lưu lượng.
- Phiên đã mất slot giữ trạng thái từ chối, không tự sống lại sau đó.
- Tắt splice ở phiên người dùng vì splice có thể đi vòng qua các buffer kiểm tra.
- Gia hạn Redis đồng bộ theo chu kỳ, các lần kiểm tra cùng IP dùng chung lần gọi
  đang thực hiện. Không tiếp tục truyền dựa trên một gia hạn nền chưa có kết quả.
- Redis TIME làm mốc thời gian chung; Lua dọn hết hạn, thu hồi IP dư khi giảm
  limit, kiểm tra slot và gia hạn trong một thao tác nguyên tử.
- Xóa UUID khỏi một node không xóa toàn bộ slot đang dùng trên các node khác.
- FailClosed=false vẫn giữ limit cục bộ khi Redis lỗi; chế độ này không bảo đảm
  limit toàn cụm trong thời gian mất kết nối Redis.

## Điều kiện để chặn trên toàn cụm

1. Nâng cấp tất cả node phục vụ UUID đó, bao gồm các node 86, 91, 93 được quan sát.
2. Tất cả dùng cùng Redis, RedisDB, KeyPrefix và ApiHost giống nhau. ApiHost là
   namespace: hai tên miền trỏ cùng một panel vẫn tạo bộ đếm riêng.
3. Bật Enable=true và FailClosed=true trên từng Node hoặc AgentConfig tương ứng.
4. Panel phải trả device_limit=2 cho UUID cần giới hạn; device_limit=0 là vô hạn.
5. Dùng cùng chính sách Expiry, RefreshInterval, HandoverGrace giữa các node.

File `config.device-limit.example.json` là ví dụ cần ghép vào cấu hình hiện có,
không thay toàn bộ cấu hình node bằng các placeholder. Port 16379 giả định đã có
tunnel bảo mật tới Redis chung. Trên VPS web đã kiểm tra, Redis bind 127.0.0.1:6379.
Không mở Redis hiện tại ra Internet để làm cấu hình mẫu này hoạt động. Thiết lập
tunnel SSH bằng tài khoản/key chỉ cho phép forward tới Redis, hoặc dùng Redis TLS
và ACL chuyên dụng. Nếu truy cập trực tiếp qua mạng, cần RedisTLS=true và CA đúng.

DB6 là cache của ezviet.xyz trong lần kiểm tra; khóa limiter có prefix riêng nên
không thay thế hay xóa ALIVE_IP_USER / ALIVE_LIST của panel. Có thể dùng database
riêng nếu tất cả node được cấu hình thống nhất. SyncEnabled=false dành cho panel
legacy không phát sự kiện device-sync; không ảnh hưởng đến giới hạn IP.

Với RefreshInterval=5, phiên đang truyền được kiểm tra Redis ít nhất mỗi 5 giây
khi có dữ liệu, cộng thời gian yêu cầu Redis. Khi giảm limit, node còn cần nhận
cập nhật user từ panel trước. Đây không phải cơ chế ngắt tất cả socket tức thời.
HandoverGrace=15 cho IP mới thay IP im lặng; đặt 0 để đợi hết Expiry=60 giây.
FailClosed=true làm gián đoạn các user có limit khi Redis/tunnel lỗi và lease cần
gia hạn. Lưu lượng qua Redis chỉ phục vụ kiểm tra slot, không chuyển dữ liệu VPN.
Việc tắt splice có thể giảm hiệu suất; cần theo dõi CPU/băng thông khi triển khai.

## Kiểm thử và build

Yêu cầu Go 1.26, GOEXPERIMENT=jsonv2. Redis trong test là miniredis riêng trong
tiến trình; test không kết nối Redis sản xuất.

```sh
GOEXPERIMENT=jsonv2 go test -count=1 -timeout=180s ./...
GOEXPERIMENT=jsonv2 go test -race -count=1 -timeout=180s ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOEXPERIMENT=jsonv2 \
  go build -trimpath -ldflags '-s -w -X github.com/wyx2685/v2node/cmd.version=4f9a4bf-device-limit.1' -o v2node-linux-amd64 .
```

Các tình huống hồi quy: 6 node tranh 2 slot; không cấu hình Redis; Redis lỗi và
khôi phục; 6 IP đang có bị giảm xuống 2; handover; clock node khác clock Redis;
xóa user trong lúc chờ Redis; xóa user trên một node không giải phóng slot của node
khác; buffer của phiên đã mở bị chặn; upload-only/final payload được xử lý đúng.

## Triển khai và kiểm tra thực tế

Trước khi thay binary, lưu bản binary/config cũ ở thư mục backup có thời gian.
Xác minh phiên bản, kiến trúc, đường dẫn service và khả năng kết nối Redis bảo mật.
Triển khai có lịch ngắn vì restart node ngắt các phiên hiện tại; chỉ một node dùng
bản mới không bảo đảm limit toàn cụm. Nếu Agent mode, panel phải hỗ trợ các API
Agent của fork Duyvj; panel legacy dùng Nodes như cấu hình hiện tại.

Sau khi tất cả node cập nhật: dùng một tài khoản thử limit=2, kết nối từ ba IP
khác nhau tới các node khác nhau. Hai IP được nhận, IP thứ ba bị chặn; nhiều kết
nối từ một IP vẫn được nhận. Thử giảm limit và ngắt/khôi phục Redis trong môi
trường thử nghiệm. Chỉ quan sát thống kê 6/2 trên panel không đủ xác minh chặn:
ALIVE_IP_USER còn dữ liệu khoảng 100–120 giây và ALIVE_LIST có cache 60 giây.

Nếu triển khai lỗi, phục hồi đúng binary/config từ backup và restart service.
Không FLUSHDB, không xóa cache IP hàng loạt. Các khóa limiter tự hết hạn.
