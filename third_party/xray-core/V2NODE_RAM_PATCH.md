# v2node RAM-only Xray patch

Nguồn vendored này là commit `wyx2685/xray-core@b17a88f9b46d`, đúng dependency
của v2node upstream tại thời điểm baseline. Các thay đổi cục bộ chỉ xử lý giới hạn
bộ nhớ, concurrency và vòng đời đóng tài nguyên; không cố ý thay đổi wire protocol.

Các khu vực được vá:

- cache attack-defense/whitelist của Shadowsocks, gồm success-cache 2.048 user,
  TTL nhàn rỗi 30 phút và xóa cache không phân biệt hoa/thường;
- session và stream AnyTLS, gồm callback hoàn tất `install-or-fire` và cleanup khi
  dispatcher từ chối stream;
- UDP worker, pubsub cleanup và periodic lifecycle theo thế hệ/single-flight;
- delayed start của VLESS reverse và toàn bộ task/worker/mux của reverse portal;
- queue/session/fragment của Hysteria và TUIC;
- request, body, stream và session của XHTTP/splitHTTP, gRPC và browser dialer;
  queue browser dialer đầy sẽ đóng connection mới thay vì block thêm goroutine;
- deadline/cancel của các connection wrapper dùng bởi transport.

Mỗi cấu trúc nhận dữ liệu từ peer đều có giới hạn hữu hạn hoặc được thay thế/dọn
khi hết vòng đời. Các regression test nằm cạnh package tương ứng và kiểm tra cả
close, timeout, queue đầy, fragment không hoàn tất và churn đồng thời.
