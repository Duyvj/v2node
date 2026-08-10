# Bảo mật triển khai

- Chỉ dùng package của tag cố định kèm SHA-256 trong release.
- Giữ `/etc/v2node/config.json` và API key ở mode `0600`.
- Dùng `--api-key-file` hoặc `--api-key-stdin`; không đặt secret trong command line.
- Không bỏ qua bước kiểm tra package, ELF và health gate của installer.
- Không dùng URL `latest` cho production.
- Với repository private, không nhúng token dài hạn vào script fleet; dùng secret
  manager hoặc kho artifact có cơ chế xác thực phù hợp.

Không commit config thật, token panel, private key, mật khẩu VPS hoặc dữ liệu nhận
dạng máy chủ vào repository này.
