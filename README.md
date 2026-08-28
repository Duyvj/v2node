# v2node (Resource-Optimized Edition)

A high-performance, resource-optimized V2Board backend based on a modified Xray-core.

Bản backend v2node tối ưu hóa tài nguyên (RAM, CPU, Goroutines, GC) dành cho V2Board, hoạt động mượt mà trên các VPS cấu hình thấp (512MB - 1GB RAM) cũng như các server tải lớn.

---

## ⚡ Các tính năng tối ưu hóa (Resource Optimizations)

- **Runtime Memory Control**: Tích hợp Soft Memory Limit (`GOMEMLIMIT`) và điều chỉnh tỷ lệ thu gom rác (`GOGC`).
- **Memory Scavenger**: Tự động thu gom và hoàn trả RAM không sử dụng về cho OS (`debug.FreeOSMemory()`).
- **Giảm Pipe Buffer Overhead**: Giảm 75% - 87% bộ nhớ đệm (16KB - 32KB thay vì 256KB-512KB mặc định).
- **DisableSniffing**: Hỗ trợ tắt domain sniffing để tiết kiệm CPU/RAM và tương thích hoàn hảo với các ứng dụng như Zalo, ngân hàng.
- **Zero-Allocation Limiter**: Loại bỏ triệt để cấp phát rác trên từng kết nối/request.
- **Leak-Free Task Runner**: Triệt tiêu rò rỉ goroutine và timer trong scheduler.
- **Kích thước Binary nhỏ gọn**: Build tối ưu stripped symbols giảm ~32% dung lượng binary.

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

Thêm mục `"Resource"` vào file `/etc/v2node/config.json`:

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

