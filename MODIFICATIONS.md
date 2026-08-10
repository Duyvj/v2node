# RAM-only patch set v0.4.4-ram2

Baseline: upstream `wyx2685/v2node` tag `v0.4.4`, commit
`2daa9dd4a114aa39294350475defa2b748d595ed`.

Mục tiêu của nhánh này chỉ là chặn tăng RAM theo thời gian. Protocol, account,
sniffing, routing, endpoint API và định dạng report traffic giữ nguyên upstream.

## Các nguồn tăng RAM đã sửa

- Task dùng một goroutine duy nhất cho mỗi lần chạy, có context hủy và `Close`
  chờ kết thúc. Task cũ không còn bị bỏ lại sau timeout/reload; timeout HTTP cục bộ
  của panel được coi là lỗi tạm thời, chỉ deadline của scheduler mới yêu cầu reload.
- Reload thay thế chính process sau khi đóng tài nguyên. Cách này thu hồi chắc chắn
  toàn bộ heap/goroutine của core cũ, kể cả goroutine trong dependency không có API
  dừng, mà không chạy hai thế hệ song song.
- Bản Xray đúng commit upstream đã được đặt tại `third_party/xray-core`. Cache,
  session, stream, packet, fragment và queue của Shadowsocks, AnyTLS, Hysteria,
  TUIC, UDP worker, XHTTP/splitHTTP, gRPC và browser dialer đều có giới hạn hữu
  hạn và đường đóng giải phóng ownership tương ứng. Periodic task dùng thế hệ
  single-flight; callback AnyTLS chịu được FIN đến sớm; VLESS/reverse timer,
  worker, mux và buffer được đóng theo owner.
- Online-IP tracker có giới hạn theo user và toàn node, giữ trạng thái ở mức cố
  định khi panel lỗi, thay map sau kỳ report thành công để trả bucket lớn cho GC,
  đồng thời tính IP mới cục bộ vào device limit. IP của kỳ trước và kỳ hiện tại
  dùng chung một quota, nên churn qua nhiều kỳ không thể cộng dồn vượt cap.
- Success-cache Shadowsocks chuẩn hóa email, xóa mọi entry khi user bị xóa và có
  trần 2.048 user cùng TTL nhàn rỗi 30 phút; cache miss/eviction vẫn fallback về
  danh sách user đầy đủ nên không đổi khả năng xác thực. Cache hit chỉ được ghi lại
  khi con trỏ user vẫn là credential hiện hành dưới read lock, nên xóa/đổi mật khẩu
  đồng thời không thể làm credential cũ sống lại. LRU refresh trễ xác nhận entry vẫn
  thuộc map dưới write lock, tránh node ma sau delete/eviction làm hỏng capacity.
- Mỗi lần reload browser dialer dùng một thế hệ queue riêng. Retire thế hệ cũ sẽ
  đánh thức waiter, drain/đóng connection chưa nhận và ngăn connection mới lọt vào
  queue cũ. Queue 256 slot từ chối/đóng connection dư theo đường không blocking;
  lỗi bind/serve không để lại trạng thái active giả. Trang browser tự lấy token mới
  sau khi process được thay thế và reconnect với exponential backoff có trần; cảnh
  báo queue đầy được gộp theo chu kỳ để tải lỗi không thể tạo log không giới hạn.
  Page/token không còn CORS wildcard và WebSocket browser chỉ nhận Origin cùng
  scheme/host/port, ngăn trang ngoài lấy token rồi chiếm queue.
- `LinkManager` dùng đăng ký nguyên tử, tự xóa khi kết nối cuối đóng, không thể tái
  tạo sau khi user bị xóa và đóng toàn bộ link khi dispatcher dừng.
- Thêm user theo giao dịch: validate trước, rollback toàn bộ batch nếu một user lỗi;
  không còn `uidMap`/Xray user mồ côi sau retry.
- Counter user đã xóa được dọn dù chưa đạt ngưỡng report; reset bằng atomic `Swap`
  để không mất byte đến đồng thời.
- HTTP response từ panel và số user có giới hạn; JSON/MessagePack user list được
  stream và kiểm tra cardinality trước khi cấp phát slice lớn. Chỉ response 2xx
  với đúng một trường `users` top-level mới có quyền thay danh sách/ETag; response
  lỗi không thể bị hiểu nhầm thành lệnh xóa toàn bộ user.
- Poll interval từ panel được parse an toàn và clamp; response stream được đọc hết
  để tái sử dụng connection thay vì tích lũy socket/TLS churn.
- Tắt Xray per-user stats trùng với counter riêng của v2node và giảm buffer mặc định
  từ 128 KiB xuống 64 KiB.
- Đóng Resty idle connections, ACME transports, config watcher, pprof server, log
  file và certificate/account file theo đúng lifecycle.
- Lỗi startup, cleanup hoặc thay process trả exit code khác 0 để systemd
  `Restart=on-failure` khởi động lại; SIGTERM sạch vẫn trả exit code 0.

## Guardrails mặc định

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

User không có device limit vẫn được kết nối khi IP tracker đầy; các IP vượt giới
hạn chỉ không được giữ lại để report. User có device limit bị từ chối khi không thể
theo dõi thêm IP một cách an toàn.

Installer tính các ngưỡng từ RAM/cgroup hiệu dụng: `GOMEMLIMIT` khoảng 45%,
`MemoryHigh` 65%, `MemoryMax` 80%, và `MemorySwapMax` 10% nhưng được clamp trong
khoảng 128–512 MiB. Installer xác minh systemd đã áp dụng đúng các giá trị này.
Trần cứng khiến systemd restart riêng service thay vì để v2node làm cạn RAM/swap
của cả VPS.

## Cài đè và rollback

- Installer dừng service v2node đang chạy và chờ nó inactive trước khi thay service,
  symlink hoặc file live, tránh để process cũ tiếp tục dùng layout đang được đổi.
- Backup format 2 có manifest SHA-256 cho mọi payload thường và manifest target cho
  symlink; parser chấp nhận đúng marker text/binary chuẩn của `sha256sum`, marker
  config vắng mặt dùng cùng tên với restore, đồng thời yêu cầu coverage không thiếu/
  trùng/traversal cho mọi payload bất biến. Backup tự xác minh trước khi dừng service,
  rollback guard được kích hoạt trước live mutation, và restore kiểm tra lại toàn bộ
  backup trước khi dùng.
- Nếu giao dịch đã thêm `/swapfile` vào `/etc/fstab`, rollback tắt swap vừa tạo và
  khôi phục nguyên bản snapshot `fstab` bằng phép thay thế nguyên tử, thay vì sửa dòng
  theo mẫu có thể làm đổi nội dung khác của máy.
- Menu quản lý và thao tác tự phục hồi menu dùng `v2node-menu` có sẵn trong release
  đã xác minh, hoạt động offline và không chạy shell từ nhánh `main` mutable. Cập nhật
  package đi qua installer đã lưu cục bộ với tag/hash được pin. Trình tạo config của
  menu ghi nguyên tử với mode `0600`, ẩn API key và từ chối host mẫu `example.com`.

## Kiểm thử bắt buộc

```text
GOEXPERIMENT=jsonv2 go mod verify
GOEXPERIMENT=jsonv2 go test ./...
GOEXPERIMENT=jsonv2 go vet ./...
```

Ngoài regression tests, patch có stress tests cho 100.000 IP khác nhau, concurrent
rotate/delete, task cancel/join/restart, user rollback, atomic traffic reporting,
LinkManager concurrency và lifecycle cache/session/queue/goroutine trong Xray.
Các giới hạn cụ thể của Xray được ghi tại
[`third_party/xray-core/V2NODE_RAM_PATCH.md`](third_party/xray-core/V2NODE_RAM_PATCH.md).
