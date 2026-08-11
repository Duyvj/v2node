# v0.4.4-ram3

RAM-fix overlay dựa trực tiếp trên `wyx2685/v2node` `v0.4.4`
(`2daa9dd4a114aa39294350475defa2b748d595ed`). Protocol, routing, sniffing,
panel API, quản lý user và định dạng report traffic giữ nguyên baseline.

Thay đổi so với `ram2`: release này trở lại đúng mô hình cài đè bản gốc.

- Chỉ thay `/usr/local/v2node/v2node` bằng binary đã vá RAM.
- Chỉ thêm systemd drop-in `90-v2node-ramfix.conf` với `GOMEMLIMIT` khoảng 45%,
  `MemoryHigh` 65%, `MemoryMax` 80% và `MemorySwapMax` 10% RAM/cgroup hiệu dụng.
- Không thay `/usr/bin/v2node`, config, geodata hoặc file `v2node.service` chính.
- Không tạo `current/releases`, swap, sysctl hay lệnh quản lý mới.
- Toàn bộ thao tác quản lý tiếp tục dùng `v2node`, giống hệt bản gốc.
- Nếu phát hiện layout `ram1/ram2`, installer dừng trước mọi thay đổi và yêu cầu cài
  lại upstream `v0.4.4`; nó không tự rollback layout cũ vì có thể ghi đè config mới.

Binary vẫn chứa đầy đủ các sửa lỗi RAM của nhánh trước:

- giới hạn online-IP, cache Shadowsocks, session/queue/packet/fragment;
- đóng đúng goroutine, timer, watcher, stream, socket và transport;
- scheduler phân biệt deadline riêng của request panel;
- browser dialer dùng queue theo thế hệ, backoff và same-origin;
- reload thay process sau graceful cleanup để thu hồi heap của core cũ;
- buffer mặc định 64 KiB và bỏ user-stat Xray trùng lặp.

Installer pin tag + SHA-256, kiểm tra cấu trúc ZIP/ELF, tạo backup format 3,
tự xác minh backup trước khi dừng service, health-check sau cài và tự rollback khi lỗi.
Các file thuộc bản gốc được snapshot hash + metadata và phải giữ nguyên qua giao dịch.

Rollback overlay:

```bash
sudo /usr/local/lib/v2node-ramfix/install.sh --rollback
```

Nên triển khai một VPS canary cho mỗi kiến trúc trước khi rollout toàn bộ fleet.
