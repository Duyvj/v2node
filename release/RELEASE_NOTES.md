# v0.4.4-ram2

Bản RAM-only dựa trực tiếp trên upstream `wyx2685/v2node` `v0.4.4`
(`2daa9dd4a114aa39294350475defa2b748d595ed`). Bản này giữ nguyên protocol,
routing, sniffing, panel API, quản lý user và report traffic; thay đổi tập trung vào
giới hạn dữ liệu sống và đóng đúng vòng đời tài nguyên.

Điểm chính:

- giới hạn cache, session, queue, fragment, packet và online-IP do mạng/panel điều khiển;
- dọn goroutine, watcher, timer, socket, transport, stream và connection khi đóng/reload;
- chặn timer chạy chồng qua `Close`/`Start`, AnyTLS FIN sớm và reverse worker sống lại sau close;
- reload bằng thay thế process sau graceful cleanup để thu hồi chắc chắn heap của core cũ;
- giảm buffer mặc định xuống 64 KiB và bỏ Xray user-stat trùng với counter của v2node;
- parser panel có giới hạn byte/user và không coi response lỗi là danh sách user rỗng;
- cap online-IP dùng chung cho state kỳ trước/hiện tại và được stress qua 128 vòng disjoint;
- success-cache Shadowsocks có cap/TTL, xóa đủ mọi IP cache của user, chuẩn hóa email
  và khóa việc ghi cache với membership hiện hành để credential vừa xóa/đổi không sống
  lại; LRU refresh xác nhận lại map entry để delete/eviction không tạo ghost node;
- timeout request panel không còn restart toàn bộ VPN; browser dialer dùng queue theo
  thế hệ, đánh thức waiter và đóng connection cũ khi retire/reload, không block khi
  queue đầy, không publish state giả sau lỗi bind/serve, tự làm mới token sau process
  replacement, reconnect có backoff, gộp cảnh báo queue đầy để chặn log flood và
  chặn Origin ngoài đọc token/chiếm queue;
- installer được pin theo tag + SHA-256, kiểm tra ELF, health gate và rollback giao
  dịch; profile RAM đặt `GOMEMLIMIT` khoảng 45%, `MemoryHigh` 65%, `MemoryMax` 80%
  RAM/cgroup hiệu dụng, cùng `MemorySwapMax` 10% clamp 128–512 MiB;
- cài đè trực tiếp lên layout bản gốc, giữ nguyên config và cung cấp symlink tương thích
  cho binary/geodata ở `/usr/local/v2node`; service cũ được dừng hẳn trước khi đổi
  service, symlink và file live;
- đóng gói menu quản lý riêng cùng `v2nodectl`; menu được phục hồi offline từ artifact
  trong package đã xác minh, không còn chạy nội dung shell từ URL/nhánh mutable, và
  tạo config mode `0600` bằng input API key ẩn thay vì dùng placeholder;
- backup/rollback thêm menu, controller, installer, binary và geodata legacy, xác minh
  checksum payload (kể cả marker `sha256sum` khác nhau giữa Linux/Windows test),
  coverage đầy đủ/không trùng, target symlink và marker config trước khi dừng service
  lẫn trước restore; rollback guard được kích hoạt trước live mutation, đồng thời khôi
  phục nguyên bản snapshot `/etc/fstab` nếu giao dịch tạo swap bị rollback.

Nên triển khai một VPS canary cho mỗi kiến trúc trước khi rollout toàn bộ fleet.
Các gói `amd64` và `arm64` có checksum trong `SHA256SUMS` đính kèm release.

Rollback luôn có thể thực hiện về release bất biến `v0.4.4-ram1` hoặc bằng
`v2nodectl rollback` nếu máy đã được cài bằng installer của repository này.
