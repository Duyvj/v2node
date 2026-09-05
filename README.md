# v2node (Resource-Optimized Edition)

A high-performance, resource-optimized V2Board / XBoard backend based on a modified Xray-core.

Bản backend v2node tối ưu hóa tài nguyên (RAM, CPU, Goroutines, GC) dành cho V2Board / XBoard, hoạt động mượt mà trên các VPS cấu hình thấp (512MB - 1GB RAM) cũng như các cụm máy chủ tải lớn.

---

## ⚡ Các tính năng nổi bật & Tối ưu hóa

- **Tối ưu hóa Kết nối & Đường truyền Siêu Ổn định (Không Gián đoạn)**:
  - **TCP KeepAlive 15s Toàn diện**: Tích hợp trực tiếp vào tầng socket của mọi Inbound và Outbound, xóa bỏ hoàn toàn hiện tượng nhà mạng di động (4G/5G Viettel, Vina, Mobi) và router NAT tự động ngắt ngầm các kết nối rảnh.
  - **ConnectionIdle 300s (5 phút)**: Giữ kết nối liên tục và bền bỉ khi người dùng đọc báo, tạm dừng stream, treo game hoặc để ứng dụng chạy nền mà không lo bị ngắt ngang.
  - **Bộ đệm tốc độ cao (Pipe Buffer 64KB - 256KB)**: Tối ưu băng thông cho mạng cáp quang và 4G/5G tốc độ cao, xem 4K/8K không bị khựng (zero buffering) hay nghẽn TCP window.
  - **Mở rộng Kết nối Đồng thời (MaxConnectionsPerUser: 1024, Global: 65536)**: Thỏa sức mở nhiều tab trình duyệt, đa thiết bị mà không bị chặn lỗi session limit.
  - **Tối ưu Linux Kernel BBR + FQ**: Tự động tinh chỉnh kernel stack (BBR congestion control, buffer TCP 16MB, backlog queue 65535) khi cài đặt qua script.
- **Quản lý tuyến đường thông minh (Sticky Balancer & Health Check)**:
  - Tự động nhận diện và gom các tuyến đường xuất trạm mặc định (**自定义默认出站** / `default_out`) từ Web Panel vào bộ cân bằng tải **Balancer**.
  - **Cố định phiên (Sticky Session 60 phút)**: Mỗi phiên người dùng (`user.Email` hoặc IP nguồn thiết bị) được giữ cố định suốt 60 phút vào cùng một cổng outbound. Khắc phục triệt để lỗi nhảy IP giữa chừng gây văng app ngân hàng, Zalo, Captcha Cloudflare, lỗi session game.
  - **Kiểm tra sức khỏe tự động (Health Check / Observatory)**: Tự động ping thăm dò (`http://cp.cloudflare.com/generate_204`) các cổng outbound song song mỗi 10 giây. Tự động chuyển hướng (Failover) mượt mà sang các cổng còn sống nếu một cổng gặp sự cố.
- **Mặc định bật DisableSniffing**: Mặc định kích hoạt `"DisableSniffing": true` trên toàn hệ thống để tiết kiệm tối đa CPU/RAM và tương thích 100% với các ứng dụng nhạy cảm (Zalo, ngân hàng, VoIP, UDP).
- **Runtime Memory Control**: Tích hợp Soft Memory Limit (`GOMEMLIMIT`) và điều chỉnh tỷ lệ thu gom rác (`GOGC`), sử dụng Go background scavenger mượt mà không gây khựng STW.
- **Zero-Allocation Limiter**: Loại bỏ triệt để cấp phát rác trên từng kết nối/request.
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

### 📖 Hướng dẫn Cấu hình Tuyến đường (Route Management) trên Web Panel

Khi bạn có **2 hoặc nhiều cổng WireGuard (hoặc SOCKS5/Shadowsocks/VLESS/...)** muốn chạy cân bằng tải (giữ session cố định + tự động failover), bạn có thể cấu hình theo 1 trong 2 cách sau:

---

#### 🌟 Cách 1: Tạo từng Tuyến đường (Route) riêng biệt trên Panel (Khuyên dùng - Trực quan, dễ quản trị)

Bạn vào **Quản lý tuyến đường** ➔ Bấm **Thêm tuyến đường** cho từng WireGuard:

##### 1. Tuyến đường 1 (Ví dụ: WireGuard Hà Nội):
- **Ghi chú**: `WG Server 1 - Ha Noi`
- **Hành động**: Chọn **自定义默认出站** (*Custom default outbound*)
- **Xray出站配置**:
```json
{
  "tag": "wg-hn-01",
  "protocol": "wireguard",
  "settings": {
    "secretKey": "SECRET_KEY_CUA_CLIENT_1_O_DAY=",
    "address": ["10.2.0.2/32"],
    "peers": [
      {
        "publicKey": "PUBLIC_KEY_SERVER_1_O_DAY=",
        "endpoint": "188.214.152.226:51820",
        "allowedIPs": ["0.0.0.0/0"],
        "keepAlive": 25
      }
    ],
    "mtu": 1280
  }
}
```

##### 2. Tuyến đường 2 (Ví dụ: WireGuard Hồ Chí Minh):
- **Ghi chú**: `WG Server 2 - Ho Chi Minh`
- **Hành động**: Chọn **自定义默认出站** (*Custom default outbound*)
- **Xray出站配置**:
```json
{
  "tag": "wg-hcm-02",
  "protocol": "wireguard",
  "settings": {
    "secretKey": "SECRET_KEY_CUA_CLIENT_2_O_DAY=",
    "address": ["10.3.0.2/32"],
    "peers": [
      {
        "publicKey": "PUBLIC_KEY_SERVER_2_O_DAY=",
        "endpoint": "103.145.2.50:51820",
        "allowedIPs": ["0.0.0.0/0"],
        "keepAlive": 25
      }
    ],
    "mtu": 1280
  }
}
```

> [!IMPORTANT]
> **Điểm mấu chốt để phân tách**:
> - Mỗi route tạo một dòng riêng biệt trên bảng danh sách của Web Panel.
> - Trường `"tag"` ở mỗi route **BẮT BUỘC PHẢI KHÁC NHAU** (ở ví dụ trên là `"wg-hn-01"` và `"wg-hcm-02"`).
> - Znode sẽ tự động nhận dạng tất cả các route có hành động `自定义默认出站`, gom lại thành cụm Balancer và kích hoạt tính năng **Sticky Session + Health Check**.

---

#### 🌟 Cách 2: Gộp chung vào 1 Tuyến đường duy nhất bằng Mảng JSON `[ ... ]`

Nếu bạn chỉ muốn tạo đúng 1 Tuyến đường trên Web Panel:
- **Ghi chú**: `Cụm Balancer 2 WireGuard Outbounds`
- **Hành động**: Chọn **自定义默认出站** (*Custom default outbound*)
- **Xray出站配置**: Dán toàn bộ cấu hình 2 WG vào trong cặp ngoặc vuông `[ ... ]`, **phân tách giữa 2 khối bằng dấu phẩy `,`** như sau:

```json
[
  {
    "tag": "wg-node-1",
    "protocol": "wireguard",
    "settings": {
      "secretKey": "SECRET_KEY_CUA_CLIENT_1=",
      "address": ["10.2.0.2/32"],
      "peers": [
        {
          "publicKey": "PUBLIC_KEY_CUA_SERVER_1=",
          "endpoint": "188.214.152.226:51820",
          "allowedIPs": ["0.0.0.0/0"],
          "keepAlive": 25
        }
      ],
      "mtu": 1280
    }
  },
  {
    "tag": "wg-node-2",
    "protocol": "wireguard",
    "settings": {
      "secretKey": "SECRET_KEY_CUA_CLIENT_2=",
      "address": ["10.3.0.2/32"],
      "peers": [
        {
          "publicKey": "PUBLIC_KEY_CUA_SERVER_2=",
          "endpoint": "103.145.2.50:51820",
          "allowedIPs": ["0.0.0.0/0"],
          "keepAlive": 25
        }
      ],
      "mtu": 1280
    }
  }
]
```

##### 🔍 Cú pháp phân tách chi tiết:
1. Mở đầu bằng dấu ngoặc vuông `[`.
2. Khối cấu hình WG thứ nhất: `{ "tag": "wg-node-1", ... }`.
3. **Dấu phẩy `,`** ngăn cách giữa hai object JSON.
4. Khối cấu hình WG thứ hai: `{ "tag": "wg-node-2", ... }`.
5. Đóng mảng bằng dấu ngoặc vuông `]`.
6. Tương tự, nếu có thêm WG thứ 3, chỉ cần thêm dấu `,` và khối `{ "tag": "wg-node-3", ... }`.

---

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
    "Profile": "standard",
    "MemLimitMB": 512,
    "GOGC": 80,
    "BufferSize": 128,
    "ConnectionIdle": 300,
    "DisableSniffing": true,
    "PeriodicMemoryReleaseInterval": 0
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
- **`"low"`**: Tối ưu cho VPS 512MB RAM (`BufferSize: 64KB`, `GOGC: 50`, `ConnectionIdle: 300s`).
- **`"standard"`** *(mặc định)*: Cân bằng lý tưởng tốc độ & ổn định tuyệt đối (`BufferSize: 128KB`, `GOGC: 80`, `ConnectionIdle: 300s`).
- **`"high_performance"`**: Dành cho máy chủ tải cực lớn (`BufferSize: 256KB`, `GOGC: 100`, `ConnectionIdle: 600s`).

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
