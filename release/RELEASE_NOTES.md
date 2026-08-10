# v0.4.4-ram1

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
- installer được pin theo tag + SHA-256, kiểm tra ELF, health gate và rollback giao dịch.

Nên triển khai một VPS canary cho mỗi kiến trúc trước khi rollout toàn bộ fleet.
Các gói `amd64` và `arm64` có checksum trong `SHA256SUMS` đính kèm release.

Rollback luôn có thể thực hiện về release bất biến `v0.4.4-p1` hoặc bằng
`v2nodectl rollback` nếu máy đã được cài bằng installer của repository này.
