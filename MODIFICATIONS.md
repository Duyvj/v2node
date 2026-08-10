# Patch set v0.4.4-personal.1

Baseline: upstream tag `v0.4.4`, commit `2daa9dd4a114aa39294350475defa2b748d595ed`.

## Runtime và control plane

- `conf/runtime.go`: thêm giới hạn polling, buffer và Go memory limit tùy chọn.
- `api/v2board/node.go`: parse interval an toàn, clamp trong khoảng cấu hình và fallback khi thiếu `base_config`.
- `api/v2board/redact.go`: che token/API key nhưng giữ `errors.Is`/`errors.As`.
- `common/task/task.go`: tránh timer cũ sau lần chạy đầu, chặn interval lỗi và làm sạch trạng thái task khi goroutine kết thúc.
- `node/task.go`: dừng chuỗi request cũ ngay khi node config đổi.

## Data plane và memory

- `core/core.go`: buffer mặc định 64 KiB; tắt Xray user stats trùng với bộ đếm riêng của v2node.
- `core/app/dispatcher/default.go`: dùng `LoadOrStore` khi tạo link manager và traffic counter.
- `limiter/limiter.go`: không cấp phát `sync.Map` mới trên mọi connection; snapshot alive-list có khóa; dữ liệu limit được thay bằng bản immutable.
- `core/user.go`: xóa counter của user đã mất và dùng atomic `Swap(0)` khi report traffic.

## Độ ổn định và bảo mật

- không panic khi response panel thiếu cấu hình nền;
- config cài đặt có mode `0600`; API key không đi qua argv/environment của jq/Python;
- self-signed private key và ACME account được ghi mode `0600`;
- sửa file-descriptor leak khi ghi certificate/account;
- installer xác minh SHA-256, ZIP entry, ELF machine, service ổn định và rollback có journal trạng thái.

## Chủ ý không thay đổi

- không thay protocol account/handshake, routing hoặc sniffing mặc định;
- không đổi API endpoint/format report của v2board;
- không đặt hard memory cap có thể chủ động OOM-kill service;
- không tự quản lý firewall và không gọi installer/menu upstream.

## Kiểm thử

```text
GOEXPERIMENT=jsonv2 go mod verify
GOEXPERIMENT=jsonv2 go test ./...
GOEXPERIMENT=jsonv2 go vet ./...
```

Có test riêng cho runtime config, interval parser, secret redaction, traffic reset/unknown-user cleanup và device-limit snapshot.
