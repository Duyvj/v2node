# v2node (Resource-Optimized Edition)

A high-performance, resource-optimized V2Board / XBoard backend based on a modified Xray-core.

Bản backend v2node tối ưu hóa tài nguyên (RAM, CPU, Goroutines, GC) dành cho V2Board / XBoard, hoạt động mượt mà trên các VPS cấu hình thấp (512MB - 1GB RAM) cũng như các cụm máy chủ tải lớn.

---

## ⚡ Các tính năng nổi bật & Tối ưu hóa

- **Quản lý tuyến đường thông minh (Sticky Balancer & Health Check)**:
  - Tự động nhận diện và gom các tuyến đường xuất trạm mặc định (**自定义默认出站** / `default_out`) từ Web Panel vào bộ cân bằng tải **Balancer**.
  - **Cố định phiên (Sticky Session)**: Mỗi phiên người dùng (`user.Email` hoặc IP nguồn thiết bị) được giữ cố định vào cùng một cổng outbound trong suốt phiên làm việc. Khắc phục triệt để lỗi nhảy IP giữa chừng gây văng app ngân hàng, Zalo, Captcha Cloudflare, lỗi session game.
  - **Kiểm tra sức khỏe tự động (Health Check / Observatory)**: Tự động ping thăm dò (`http://cp.cloudflare.com/generate_204`) các cổng outbound song song mỗi 10 giây. Tự động chuyển hướng (Failover) mượt mà sang các cổng còn sống nếu một cổng gặp sự cố.
  - **Dọn dẹp phiên tự động**: Tự động giải phóng bộ nhớ cho các phiên không hoạt động quá 10 phút.
- **Mặc định bật DisableSniffing**: Mặc định kích hoạt `"DisableSniffing": true` trên toàn hệ thống để tiết kiệm tối đa CPU/RAM và tương thích 100% với các ứng dụng nhạy cảm (Zalo, ngân hàng, VoIP, UDP).
- **Runtime Memory Control**: Tích hợp Soft Memory Limit (`GOMEMLIMIT`) và điều chỉnh tỷ lệ thu gom rác (`GOGC`).
- **Memory Scavenger**: Tự động thu gom và hoàn trả RAM không sử dụng về cho OS (`debug.FreeOSMemory()`).
- **Giảm Pipe Buffer Overhead**: Giảm 75% - 87% bộ nhớ đệm (16KB - 32KB thay vì 256KB-512KB mặc định).
- **Zero-Allocation Limiter**: Loại bỏ triệt để cấp phát rác trên từng kết nối/request.
- **Leak-Free Task Runner**: Triệt tiêu rò rỉ goroutine và timer trong scheduler.
- **Kích thước Binary nhỏ gọn**: Build tối ưu stripped symbols giảm ~32% dung lượng binary.

---

## 🌐 Mô hình Luồng Kết nối & Cân bằng tải

```
[ Người dùng ] ──► [ v2node ] ──► [ Balancer + Health Check ]
                                            │
               ┌────────────────────────────┼────────────────────────────┐
               ▼                            ▼                            ▼
     Phiên 1, giữ cố định         Phiên 2, giữ cố định         Phiên 3, giữ cố định
               │                            │                            │
               ▼                            ▼                            ▼
            [ WG-A ]                     [ WG-B ]                     [ WG-C ]
               │                            │                            │
               └────────────────────────────┼────────────────────────────┘
                                            ▼
                                       [ Internet ]
```

### Cách cấu hình trên Web Panel (V2Board / XBoard):
1. Vào mục **Quản lý tuyến đường** (Route Management).
2. Thêm route mới với:
   - **Ghi chú**: Đặt tên ghi chú (ví dụ: `Proton VN 1`, `WARP 1`, v.v.).
   - **Hành động**: Chọn **自定义默认出站** (Custom default outbound).
   - **Xray出站配置**: Dán cấu hình Outbound JSON của WireGuard (hoặc giao thức mong muốn):
     ```json
     {
       "tag": "38454722",
       "protocol": "wireguard",
       "settings": {
         "secretKey": "YOUR_SECRET_KEY",
         "address": ["10.2.0.2/32"],
         "peers": [
           {
             "publicKey": "SERVER_PUBLIC_KEY",
             "endpoint": "188.214.152.226:51820",
             "allowedIPs": ["0.0.0.0/0"],
             "keepAlive": 25
           }
         ],
         "mtu": 1280
       }
     }
     ```
   *(Hệ thống hỗ trợ cấu hình nhiều route riêng biệt hoặc dán mảng JSON `[ { ... }, { ... } ]`)*.

---

## 🚀 Cài đặt nhanh (Quick Install)

### 1. Cài đặt 1 lệnh (One-Click Install)

```bash
wget -N https://raw.githubusercontent.com/Duyvj/v2node/main/script/install.sh && bash install.sh
```

Hoặc qua `curl`:

```bash
bash <(curl -Ls https://raw.githubusercontent.com/Duyvj/v2node/main/script/install.sh)
```

---

## ⚙️ Cấu hình Tối ưu Tài nguyên (Resource Configuration)

Mục `"Resource"` trong file `/etc/v2node/config.json`:

```json
{
  "Log": {
    "Level": "warning",
    "Output": "",
    "Access": "none"
  },
  "Resource": {
    "Profile": "low",
    "MemLimitMB": 256,
    "GOGC": 50,
    "BufferSize": 16,
    "ConnectionIdle": 45,
    "DisableSniffing": true,
    "PeriodicMemoryReleaseInterval": 60
  },
  "Nodes": [
    {
      "ApiHost": "https://your-panel.com",
      "ApiKey": "your_api_token",
      "NodeID": 1,
      "Timeout": 15,
      "DisableSniffing": true
    }
  ]
}
```

### Các Profile sẵn có:
- **`"low"`**: Tối ưu tối đa cho VPS 512MB RAM (`BufferSize: 16KB`, `GOGC: 50`, `ConnectionIdle: 45s`).
- **`"standard"`** *(mặc định)*: Cân bằng hiệu năng và tài nguyên (`BufferSize: 32KB`, `GOGC: 80`, `ConnectionIdle: 60s`).
- **`"high_performance"`**: Dành cho server băng thông cực lớn (`BufferSize: 128KB`, `GOGC: 100`, `ConnectionIdle: 120s`).

---

## 🛠️ Biên dịch từ mã nguồn (Build from Source)

Yêu cầu: Go >= 1.26 (với cờ `GOEXPERIMENT=jsonv2`).

### Linux / macOS:
```bash
./build.sh
```

### Windows (PowerShell):
```powershell
.\build.ps1
```

---

## 📜 Quản lý dịch vụ

```bash
v2node              # Menu quản lý tương tác
v2node start        # Khởi động dịch vụ
v2node stop         # Dừng dịch vụ
v2node restart      # Khởi động lại
v2node status       # Kiểm tra trạng thái
v2node log          # Xem log thời gian thực
```
