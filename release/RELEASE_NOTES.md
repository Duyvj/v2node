# v0.4.4-ram5

RAM-fix overlay dựa trực tiếp trên `wyx2685/v2node` `v0.4.4`
(`2daa9dd4a114aa39294350475defa2b748d595ed`). Protocol, routing, sniffing,
panel API, quản lý user và định dạng report traffic giữ nguyên baseline.

## Thay đổi so với ram3

`ram3` dùng các tỷ lệ cố định `45% / 65% / 80%`. Trên VPS có nhiều kết nối,
`GOMEMLIMIT=45%` có thể làm Go GC chạy dày sớm và `MemoryHigh=65%` có thể kích
hoạt reclaim/throttling trước khi máy thực sự thiếu RAM.

`ram5` giữ profile capacity thích ứng đã có ở `ram4`, đồng thời sửa cách đọc
cgroup v2, từ chối parent cgroup hạ trần thực tế và bổ sung regression test:

- chừa cho hệ điều hành `max(384 MiB, 15% RAM/cgroup hiệu dụng)`, nhưng không quá
  25% tổng RAM trên VPS nhỏ;
- `MemoryMax` là phần RAM còn lại sau host reserve;
- `GOMEMLIMIT` thấp hơn trần service `max(256 MiB, 10%)`, với headroom được cap
  ở một phần ba trần trên VPS nhỏ;
- `MemoryHigh` thấp hơn trần service `max(128 MiB, 5%)`, với headroom được cap
  ở một phần tư trần trên VPS nhỏ, nên chỉ là ngưỡng áp lực gần khẩn cấp;
- `MemorySwapMax` giữ 10%, clamp 128–512 MiB và không tự tạo swap.

| RAM hiệu dụng | Host reserve | GOMEMLIMIT | MemoryHigh | MemoryMax |
|---:|---:|---:|---:|---:|
| 2 GiB | 384 MiB | 1408 MiB | 1536 MiB | 1664 MiB |
| 4 GiB | 614 MiB | 3134 MiB | 3308 MiB | 3482 MiB |

Profile mới cho phép heap/working set lớn hơn rõ rệt khi nhiều người dùng đồng thời,
nhưng vẫn giữ trần khẩn cấp để tránh một service làm cạn RAM toàn VPS. Chạm
`MemoryMax` vẫn có thể khiến systemd restart v2node và rớt phiên; đây là lớp bảo vệ
cuối, không phải ngưỡng vận hành bình thường.

Nếu config đang đặt `Runtime.MemoryLimit`, giá trị đó vẫn được tôn trọng và thay
`GOMEMLIMIT` của drop-in. Cần chạy lại installer sau khi resize RAM VPS.

## Phạm vi không đổi

- Chỉ thay `/usr/local/v2node/v2node` và drop-in `90-v2node-ramfix.conf`.
- Không thay `/usr/bin/v2node`, config, geodata hoặc file service chính.
- Không tạo `current/releases`, swap, sysctl hay CLI mới.
- Quản lý tiếp tục dùng lệnh `v2node` của bản gốc.
- Toàn bộ giới hạn state, cleanup goroutine/timer/socket/transport và các sửa race
  của ram3 vẫn được giữ nguyên.
- Máy đang chạy ram3/ram4 có thể cài đè trực tiếp; layout ram1/ram2 vẫn phải được đưa về
  upstream `v0.4.4` trước.

Rollback overlay:

```bash
sudo /usr/local/lib/v2node-ramfix/install.sh --rollback
```

Nên kiểm thử một VPS canary với đúng lượng người dùng cao điểm trước khi rollout.
