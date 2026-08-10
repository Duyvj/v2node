# Triển khai nhiều VPS

## Quy trình khuyến nghị

1. Tạo `/root/v2node-config.json` mode `0600` trên từng VPS.
2. Cài một máy canary cho mỗi kiến trúc CPU.
3. Kiểm tra `v2nodectl status`, `v2nodectl version` và journal.
4. Rollout một batch nhỏ, theo dõi RAM, restart count và report traffic.
5. Chỉ triển khai toàn fleet khi batch nhỏ ổn định.

Luôn pin tag, URL và SHA-256. Không dùng release `latest`, không tải rồi chạy installer upstream, và không đưa API key vào argv hoặc shell history.

## Hash release v0.4.4-p1

```text
amd64  ec72782211e683c1804bb4f56f1cb4240a4e29236491b9c722beb9276299dd2f
arm64  76d373985fe8695d2612b1de2878d510396b46960ebbf313d4f1eda28f76d490
```

## Rollback

```bash
v2nodectl rollback
v2nodectl status
```

Installer cũng tự rollback khi package, ELF, service start hoặc health gate không đạt yêu cầu.
