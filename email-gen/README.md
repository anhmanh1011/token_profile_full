# email-gen

CLI Rust sinh email cross-product từ `domains.txt × usernames.txt`. Tối ưu cho throughput cao, RAM thấp, pipeline parallel với mmap + rayon + writer thread.

Đặt ngang hàng với `Manage_User/` (Python) và `Get_Profile/` (Go) trong monorepo `token_profile_full`. Output tương thích trực tiếp với `Get_Profile/reader/file.go`.

## Mục tiêu hiệu năng

- Input: 1,000,000 domains × 200 usernames = 200,000,000 emails (~10 GB plain)
- Target: < 60 s trên 8 core NVMe, RAM peak < 500 MB

## Kiến trúc

```
domains.txt (mmap) ──► memchr scan ──► Vec<(u32,u32) offsets> ──► [optional dedup HashSet<&[u8]>]
                                                                            │
                                               ┌─── [optional shuffle chunk order] ────┐
                                               │                                       │
usernames.txt ──► Arc<Vec<String>> ────────────┤                                       │
                                               ▼                                       │
                              rayon::par_iter over chunks ◄────────────────────────────┘
                                               │
                              build_chunk → Vec<u8> (extend_from_slice, preallocated)
                                               │
                              crossbeam bounded(16) + byte-budget 256 MB
                                               │
                                               ▼
                                   Writer thread (BufWriter 64 MB)
                                               │
                              ┌────────────────┼─────────────────────┐
                              ▼                ▼                     ▼
                           File            GzEncoder(File)    Splitter rotating
```

## Cài đặt

Cần Rust ≥ 1.74 (Edition 2021).

```bash
# Nếu chưa có Rust, cài rustup:
#   Windows: tải từ https://rustup.rs (rustup-init.exe)
#   Linux:   curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh

cd email-gen
cargo build --release
# Binary: target/release/email-gen  (Linux/macOS) hoặc email-gen.exe (Windows)
```

## Sử dụng

```bash
email-gen [OPTIONS]

Options:
  -d, --domains <FILE>       File domains (1/line)         [default: domains.txt]
  -u, --usernames <FILE>     File usernames (1/line)       [default: usernames.txt]
  -o, --output <FILE>        File output                    [default: emails.txt]
  -c, --chunk-size <N>       Domains mỗi chunk              [default: 2000]
  -t, --threads <N>          Số worker threads (0=auto)     [default: 0]
  -b, --buffer-size <MB>     BufWriter buffer               [default: 64]
      --split <SIZE_GB>      Split output mỗi N GB
      --gzip                 Gzip output
      --dedup                Dedup domains (xem ghi chú)
      --shuffle              Shuffle thứ tự chunks
      --format <FORMAT>      plain | csv | json             [default: plain]
      --progress             Progress bar
      --dry-run              Compute + count, không ghi file
  -v, --verbose
  -h, --help
  -V, --version
```

### Ví dụ nhỏ (smoke test)

```bash
mkdir -p testdata
seq 1 100 | awk '{print "domain"$1".com"}' > testdata/domains.txt
seq 1 50  | awk '{print "user"$1}'         > testdata/usernames.txt

./target/release/email-gen \
    -d testdata/domains.txt \
    -u testdata/usernames.txt \
    -o /tmp/emails.txt \
    --progress -v

wc -l /tmp/emails.txt   # expect 5000
head /tmp/emails.txt    # user1@domain1.com, user2@domain1.com, ...
```

### Full-scale run (stress test)

```bash
# Tạo 1M domains + 200 usernames
seq 1 1000000 | awk '{print "domain"$1".example.com"}' > testdata/domains.txt
seq 1 200     | awk '{print "user"$1}'                 > testdata/usernames.txt

./target/release/email-gen \
    -d testdata/domains.txt \
    -u testdata/usernames.txt \
    -o /tmp/emails.txt \
    --progress
# Expected: ~10 GB, ~50-60s trên 8-core NVMe, RAM < 500 MB
```

## Ghi chú design

### Dedup chỉ dedupe domains

`--dedup` dedupe domains trước khi cross-product, **không** dedupe output. Lý do: với `domains × usernames` = 200M, HashSet cho email sẽ ngốn > 10 GB RAM, vi phạm target. Nếu `domains.txt` và `usernames.txt` đều unique thì cross-product đã là unique.

### Shuffle chỉ shuffle thứ tự chunks

Full shuffle 200M rows cần load toàn bộ vào RAM. Thay vào đó, shuffle thứ tự chunk được ghi ra; order trong mỗi chunk giữ nguyên. Kết quả "trông" random, RAM vẫn bounded.

### Tại sao chunk-size default 2000 (không phải 20000)?

Với 200 usernames × 20000 domains × ~35 bytes ≈ 140 MB mỗi chunk. 8 workers × 140 MB = > 1 GB trong flight, vượt 500 MB target.

Default 2000 cho ~14 MB/chunk, 16-slot channel × ~14 MB + 256 MB byte budget ≈ 400 MB total. Nếu chấp nhận RAM cao hơn (ví dụ usernames ít), override `-c 20000`.

### UTF-8 validation

`usernames.txt` được validate UTF-8 (nhỏ, ~200 items). `domains.txt` **không** validate — bytes được copy byte-for-byte ra output. Điều này cho phép domain unicode (`münchen.de`, `日本.jp`) được giữ nguyên.

### Gzip compression level

Mặc định level 1 (Fastest). Level 6 (default của flate2) sẽ vượt 60 s budget cho 10 GB output.

## Output formats

| Format | Header? | Dòng mẫu |
|--------|---------|----------|
| `plain` | không | `alice@example.com` |
| `csv`   | `username,domain,email` | `alice,example.com,alice@example.com` |
| `json`  | không (JSON Lines) | `{"u":"alice","d":"example.com","e":"alice@example.com"}` |

Tất cả line ending là LF (`\n`). Compat với [Get_Profile/reader/file.go](../Get_Profile/reader/file.go) (trim CRLF khi đọc).

## Test

```bash
cargo test --release                    # unit + integration
cargo test --release -- --ignored       # stress test 1M × 200
cargo clippy --release -- -D warnings   # lint
cargo fmt --check                       # format check
```

### Test coverage

```bash
cargo install cargo-llvm-cov
cargo llvm-cov --html
```

## Benchmark

```bash
cargo bench   # criterion, vary chunk_size ∈ {500, 2000, 5000, 20000}
# HTML report: target/criterion/report/index.html
```

## RAM peak measurement

- **Linux**: `/usr/bin/time -v ./target/release/email-gen ...` → dòng "Maximum resident set size".
- **Windows PowerShell**:
  ```powershell
  $p = Start-Process -FilePath .\target\release\email-gen.exe -ArgumentList '-d','domains.txt' -PassThru
  while(!$p.HasExited){ $p.Refresh(); $p.PeakWorkingSet64/1MB; Start-Sleep -Milliseconds 250 }
  ```

Ngoài ra, tool cũng tự in `🔋 RAM peak` cuối stats (đọc `/proc/self/status VmHWM` trên Linux, `GetProcessMemoryInfo` trên Windows).

## Các optimization đã áp dụng

1. **mmap zero-copy**: domains.txt mmap thay vì read-into-Vec. Với 1M domains ~25 MB, tiết kiệm 1 copy + bỏ qua overhead BufReader.
2. **memchr SIMD scan**: `memchr::memchr_iter` dùng SIMD (AVX2/SSE2) tìm `\n` — nhanh hơn loop byte-by-byte ~5-10x.
3. **u32 line offsets**: `(u32, u32)` thay `(usize, usize)` tiết kiệm 50% memory cho offsets table với file < 4GB.
4. **Zero-allocation dedup**: `HashSet<&[u8]>` keyed by mmap slice — không allocate String cho từng domain.
5. **Vec<u8> preallocated**: mỗi chunk ước lượng chính xác `est = chunks × users × avg_bytes`, `extend_from_slice` thay `format!` — tránh Fmt trait overhead và realloc.
6. **Byte budget backpressure**: `crossbeam bounded(16)` + shared `AtomicUsize` giới hạn tổng 256 MB in flight — ngăn worker build quá nhanh vượt RAM budget.
7. **Arc<Vec<String>> share usernames**: usernames share read-only across threads, không clone.
8. **Dedicated writer thread**: workers parallelism không bị block bởi disk I/O; writer có BufWriter 64 MB để amortize syscalls.
9. **Gzip level 1**: ưu tiên tốc độ cho stream 10 GB.
10. **Rayon work-stealing**: tự cân bằng chunks giữa cores, không cần phân phối thủ công.
11. **Shuffle chunk-index chỉ**: không move chunks data, chỉ shuffle Vec<usize> index → O(n_chunks) swap.
12. **rust-toolchain release profile**: `lto=true`, `codegen-units=1`, `opt-level=3`, `strip=true` — tối đa inlining cross-crate.

## Module structure

| File | Trách nhiệm |
|------|-------------|
| [src/main.rs](src/main.rs) | Entry point, wiring |
| [src/lib.rs](src/lib.rs) | Re-export modules |
| [src/config.rs](src/config.rs) | Cli parsing + Config validation |
| [src/reader.rs](src/reader.rs) | Mmap + line_offsets + dedup + load_usernames |
| [src/generator.rs](src/generator.rs) | Core cross-product + ByteBudget |
| [src/writer.rs](src/writer.rs) | Writer thread + spawn + effective_output_path |
| [src/splitter.rs](src/splitter.rs) | File rotation + gzip wrapping + SingleFile/Splitter |
| [src/stats.rs](src/stats.rs) | Stats struct + print_vi + ram_peak (cross-platform) |
| [src/error.rs](src/error.rs) | thiserror enums cho tất cả modules |
| [benches/generator_bench.rs](benches/generator_bench.rs) | Criterion bench vary chunk_size |
| [tests/integration_test.rs](tests/integration_test.rs) | End-to-end tests |

## Error handling

- File not found → `ReaderError::NotFound` + exit ≠ 0
- Permission denied → `ReaderError::PermissionDenied`
- Invalid UTF-8 trong usernames → `ReaderError::InvalidUtf8 { line }`
- Disk full → match `raw_os_error()` 28 (Linux) / 112 (Windows) / `WriteZero` → `WriterError::DiskFull`
- Writer panic → main join handle trả `anyhow` error + stack trace
- Empty inputs → `GenError::UsernameEmpty` / `GenError::DomainEmpty`

## License

MIT
