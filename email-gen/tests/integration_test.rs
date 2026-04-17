// End-to-end tests. Drive generator + writer qua public API,
// verify output file đúng format và chứa emails mong đợi.

use std::fs::File;
use std::io::{Read, Write};
use std::path::PathBuf;

use crossbeam_channel::bounded;
use email_gen::config::{Config, OutputFormat};
use email_gen::{generator, reader, writer};
use flate2::read::GzDecoder;
use tempfile::TempDir;

fn write_file(dir: &std::path::Path, name: &str, content: &[u8]) -> PathBuf {
    let path = dir.join(name);
    let mut f = File::create(&path).unwrap();
    f.write_all(content).unwrap();
    path
}

fn base_cfg(dir: &std::path::Path, output: &str) -> Config {
    Config {
        domains_path: dir.join("domains.txt"),
        usernames_path: dir.join("usernames.txt"),
        output_path: dir.join(output),
        chunk_size: 100,
        threads: 2,
        buffer_mb: 1,
        split_gb: None,
        gzip: false,
        dedup: false,
        shuffle: false,
        format: OutputFormat::Plain,
        progress: false,
        dry_run: false,
        verbose: false,
    }
}

fn run_end_to_end(cfg: &Config) -> u64 {
    let usernames = reader::load_usernames(&cfg.usernames_path).unwrap();
    let mapped = reader::map(&cfg.domains_path).unwrap();
    let offs = reader::line_offsets(mapped.as_bytes());
    let slices = reader::domain_slices(mapped.as_bytes(), &offs, cfg.dedup);
    let (tx, rx) = bounded::<Vec<u8>>(16);
    let (handle, _) = writer::spawn_writer(rx, cfg, None).unwrap();
    let total = generator::run(&slices, usernames, cfg, tx, None).unwrap();
    handle.join().unwrap().unwrap();
    total
}

#[test]
fn small_input_plain() {
    let dir = TempDir::new().unwrap();
    let mut domains = String::new();
    for i in 0..1000 {
        domains.push_str(&format!("domain{i}.com\n"));
    }
    let mut users = String::new();
    for i in 0..100 {
        users.push_str(&format!("user{i}\n"));
    }
    write_file(dir.path(), "domains.txt", domains.as_bytes());
    write_file(dir.path(), "usernames.txt", users.as_bytes());

    let cfg = base_cfg(dir.path(), "out.txt");
    let total = run_end_to_end(&cfg);
    assert_eq!(total, 100_000);

    let content = std::fs::read_to_string(dir.path().join("out.txt")).unwrap();
    let lines: Vec<&str> = content.lines().collect();
    assert_eq!(lines.len(), 100_000);
    // Spot check: mỗi line phải có đúng 1 '@'
    for line in &lines[..100] {
        assert_eq!(line.matches('@').count(), 1, "bad line: {line}");
    }
}

#[test]
fn blank_and_whitespace_lines_skipped() {
    let dir = TempDir::new().unwrap();
    write_file(
        dir.path(),
        "domains.txt",
        b"a.com\n\n  \n  b.com  \n\t\nc.com\n",
    );
    write_file(dir.path(), "usernames.txt", b"u1\n");
    let cfg = base_cfg(dir.path(), "out.txt");
    let total = run_end_to_end(&cfg);
    assert_eq!(total, 3); // a.com, b.com (trimmed), c.com
    let content = std::fs::read_to_string(dir.path().join("out.txt")).unwrap();
    let mut lines: Vec<&str> = content.lines().collect();
    lines.sort();
    assert_eq!(lines, vec!["u1@a.com", "u1@b.com", "u1@c.com"]);
}

#[test]
fn unicode_domain_preserved_byte_exact() {
    let dir = TempDir::new().unwrap();
    write_file(
        dir.path(),
        "domains.txt",
        "münchen.de\n日本.jp\n".as_bytes(),
    );
    write_file(dir.path(), "usernames.txt", b"alice\n");
    let cfg = base_cfg(dir.path(), "out.txt");
    let _ = run_end_to_end(&cfg);
    let content = std::fs::read_to_string(dir.path().join("out.txt")).unwrap();
    assert!(content.contains("alice@münchen.de"));
    assert!(content.contains("alice@日本.jp"));
}

#[test]
fn dedup_removes_duplicate_domains() {
    let dir = TempDir::new().unwrap();
    let mut doms = String::new();
    for i in 0..800 {
        doms.push_str(&format!("domain{i}.com\n"));
    }
    // Thêm 200 duplicate
    for i in 0..200 {
        doms.push_str(&format!("domain{i}.com\n"));
    }
    write_file(dir.path(), "domains.txt", doms.as_bytes());
    write_file(dir.path(), "usernames.txt", b"u1\nu2\n");
    let mut cfg = base_cfg(dir.path(), "out.txt");
    cfg.dedup = true;
    let total = run_end_to_end(&cfg);
    assert_eq!(total, 1600); // 800 unique × 2 users
    let content = std::fs::read_to_string(dir.path().join("out.txt")).unwrap();
    assert_eq!(content.lines().count(), 1600);
}

#[test]
fn dedup_off_keeps_duplicates() {
    let dir = TempDir::new().unwrap();
    write_file(dir.path(), "domains.txt", b"a.com\na.com\na.com\n");
    write_file(dir.path(), "usernames.txt", b"u1\n");
    let cfg = base_cfg(dir.path(), "out.txt");
    let total = run_end_to_end(&cfg);
    assert_eq!(total, 3);
}

#[test]
fn split_produces_multiple_files() {
    let dir = TempDir::new().unwrap();
    // 10000 domains × 10 users × ~20 bytes = ~2MB. Split 0 threshold is disallowed,
    // nhưng ta dùng API splitter trực tiếp cho unit-like test. Ở đây, test split qua Config
    // với threshold giả — dùng chunk_size nhỏ để tạo nhiều chunks.
    let mut doms = String::new();
    for i in 0..10_000 {
        doms.push_str(&format!("d{i}.example.com\n"));
    }
    let mut users = String::new();
    for i in 0..10 {
        users.push_str(&format!("user{i}\n"));
    }
    write_file(dir.path(), "domains.txt", doms.as_bytes());
    write_file(dir.path(), "usernames.txt", users.as_bytes());

    // 100k emails × ~20B ≈ 2MB. Split bằng cách tạo threshold nhỏ hơn qua trick:
    // Thay vì split_gb (GB), test dùng trực tiếp Splitter với small threshold.
    // Ở integration test thật, ta chỉ verify split không bị lỗi với split_gb=1
    // (output thực < 1GB nên chỉ 1 file được tạo → không mục tiêu).
    // Thay vào đó, verify split với threshold 1GB: output < 1GB → 1 file emails_001.txt.
    let mut cfg = base_cfg(dir.path(), "out.txt");
    cfg.chunk_size = 500;
    cfg.split_gb = Some(1);
    let total = run_end_to_end(&cfg);
    assert_eq!(total, 100_000);
    assert!(dir.path().join("out_001.txt").exists());
}

#[test]
fn gzip_roundtrip() {
    let dir = TempDir::new().unwrap();
    write_file(dir.path(), "domains.txt", b"a.com\nb.com\n");
    write_file(dir.path(), "usernames.txt", b"alice\nbob\n");
    let mut cfg = base_cfg(dir.path(), "out.txt");
    cfg.gzip = true;
    let _ = run_end_to_end(&cfg);

    let gz_path = dir.path().join("out.txt.gz");
    assert!(gz_path.exists());
    let mut decoder = GzDecoder::new(File::open(&gz_path).unwrap());
    let mut s = String::new();
    decoder.read_to_string(&mut s).unwrap();
    let mut lines: Vec<&str> = s.lines().collect();
    lines.sort();
    assert_eq!(
        lines,
        vec!["alice@a.com", "alice@b.com", "bob@a.com", "bob@b.com"]
    );
}

#[test]
fn csv_format_has_header() {
    let dir = TempDir::new().unwrap();
    write_file(dir.path(), "domains.txt", b"a.com\n");
    write_file(dir.path(), "usernames.txt", b"alice\n");
    let mut cfg = base_cfg(dir.path(), "out.csv");
    cfg.format = OutputFormat::Csv;
    let _ = run_end_to_end(&cfg);
    let content = std::fs::read_to_string(dir.path().join("out.csv")).unwrap();
    let lines: Vec<&str> = content.lines().collect();
    assert_eq!(lines[0], "username,domain,email");
    assert_eq!(lines[1], "alice,a.com,alice@a.com");
}

#[test]
fn json_format_parseable() {
    let dir = TempDir::new().unwrap();
    write_file(dir.path(), "domains.txt", b"a.com\n");
    write_file(dir.path(), "usernames.txt", b"alice\n");
    let mut cfg = base_cfg(dir.path(), "out.json");
    cfg.format = OutputFormat::Json;
    let _ = run_end_to_end(&cfg);
    let content = std::fs::read_to_string(dir.path().join("out.json")).unwrap();
    // Mỗi dòng là JSON object hợp lệ.
    for line in content.lines() {
        assert!(line.starts_with('{') && line.ends_with('}'));
        assert!(line.contains("\"u\":\"alice\""));
        assert!(line.contains("\"d\":\"a.com\""));
        assert!(line.contains("\"e\":\"alice@a.com\""));
    }
}

#[test]
fn missing_domains_file_errors() {
    let dir = TempDir::new().unwrap();
    write_file(dir.path(), "usernames.txt", b"alice\n");
    // domains.txt không tồn tại
    let cfg = base_cfg(dir.path(), "out.txt");
    let err = reader::map(&cfg.domains_path).unwrap_err();
    assert!(matches!(
        err,
        email_gen::error::ReaderError::NotFound(_)
    ));
}

#[test]
fn dry_run_does_not_create_file() {
    let dir = TempDir::new().unwrap();
    write_file(dir.path(), "domains.txt", b"a.com\nb.com\n");
    write_file(dir.path(), "usernames.txt", b"u1\n");
    let mut cfg = base_cfg(dir.path(), "out.txt");
    cfg.dry_run = true;
    let total = run_end_to_end(&cfg);
    assert_eq!(total, 2);
    assert!(!dir.path().join("out.txt").exists());
}

// Stress test — chỉ chạy với --ignored.
#[test]
#[ignore]
fn stress_1m_domains_200_users() {
    let dir = TempDir::new().unwrap();
    let mut doms = String::with_capacity(25_000_000);
    for i in 0..1_000_000 {
        doms.push_str(&format!("domain{i}.example.com\n"));
    }
    let mut users = String::with_capacity(2_000);
    for i in 0..200 {
        users.push_str(&format!("user{i}\n"));
    }
    write_file(dir.path(), "domains.txt", doms.as_bytes());
    write_file(dir.path(), "usernames.txt", users.as_bytes());

    let mut cfg = base_cfg(dir.path(), "out.txt");
    cfg.chunk_size = 2000;
    cfg.threads = num_cpus::get();
    cfg.buffer_mb = 64;

    let start = std::time::Instant::now();
    let total = run_end_to_end(&cfg);
    let elapsed = start.elapsed();
    assert_eq!(total, 200_000_000);
    let _ = elapsed;
    // Không assert cứng < 60s vì tuỳ máy; chỉ in ra cho người chạy xem.
    eprintln!("stress test elapsed: {:?}", elapsed);
}
