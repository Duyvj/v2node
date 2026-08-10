# Bảo mật triển khai

- Chỉ dùng package kèm SHA-256 cố định.
- Giữ config và API key ở mode `0600`.
- Dùng `--api-key-file` hoặc `--api-key-stdin`; không đặt secret trong command line.
- Không sửa installer để bỏ package/ELF/health validation.
- Không chạy URL `latest` trong production.
- Với repository private, không nhúng token dài hạn vào script fleet; dùng storage hoặc secret manager phù hợp.

Không commit config thật, token panel, private key hoặc dữ liệu nhận dạng VPS vào repository này.
