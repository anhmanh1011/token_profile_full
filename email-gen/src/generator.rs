// Core logic: cross-product domains × usernames.
// Rayon workers build Vec<u8> per chunk, send qua crossbeam channel với byte-budget backpressure.

use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Condvar, Mutex};

use crossbeam_channel::Sender;
use indicatif::ProgressBar;
use rand::seq::SliceRandom;
use rand::thread_rng;
use rayon::prelude::*;

use crate::config::{Config, OutputFormat};
use crate::error::GenError;

/// Byte budget — giới hạn tổng bytes đang in flight (chưa được writer tiêu thụ).
/// Tạo backpressure mềm hơn channel slot count: một chunk to vẫn block nếu vượt budget.
pub struct ByteBudget {
    queued: Mutex<usize>,
    cv: Condvar,
    max: usize,
}

impl ByteBudget {
    pub fn new(max: usize) -> Self {
        Self {
            queued: Mutex::new(0),
            cv: Condvar::new(),
            max,
        }
    }

    pub fn acquire(&self, n: usize) {
        let mut q = self.queued.lock().expect("budget mutex poisoned");
        // Cho phép chunk lớn hơn max đi qua riêng lẻ để tránh deadlock.
        while *q > 0 && *q + n > self.max {
            q = self.cv.wait(q).expect("budget cv");
        }
        *q += n;
    }

    pub fn release(&self, n: usize) {
        let mut q = self.queued.lock().expect("budget mutex poisoned");
        *q = q.saturating_sub(n);
        self.cv.notify_all();
    }

    pub fn current(&self) -> usize {
        *self.queued.lock().expect("budget mutex poisoned")
    }
}

/// Ước lượng bytes trung bình / email cho preallocation.
pub(crate) fn avg_bytes_per_email(
    usernames: &[String],
    domains: &[&[u8]],
    fmt: OutputFormat,
) -> usize {
    if usernames.is_empty() || domains.is_empty() {
        return 0;
    }
    // Sample 16 items để estimate nhanh.
    let n = usernames.len().min(16);
    let avg_u: usize = usernames.iter().take(n).map(|s| s.len()).sum::<usize>() / n;
    let m = domains.len().min(16);
    let avg_d: usize = domains.iter().take(m).map(|d| d.len()).sum::<usize>() / m;
    match fmt {
        // "user@domain\n" = avg_u + 1 + avg_d + 1
        OutputFormat::Plain => avg_u + avg_d + 2,
        // "user,domain,user@domain\n" = 2*avg_u + 2*avg_d + 4
        OutputFormat::Csv => 2 * avg_u + 2 * avg_d + 4,
        // {"u":"U","d":"D","e":"U@D"}\n — ~22 bytes overhead + 2*avg_u + 2*avg_d + 1
        OutputFormat::Json => 2 * avg_u + 2 * avg_d + 23,
    }
}

/// Build Vec<u8> cho 1 chunk domains. Pre-allocate chính xác capacity để không realloc.
pub(crate) fn build_chunk(
    slices: &[&[u8]],
    users: &[String],
    fmt: OutputFormat,
    est_bytes: usize,
) -> Vec<u8> {
    let mut buf = Vec::with_capacity(est_bytes);
    match fmt {
        OutputFormat::Plain => {
            for d in slices {
                for u in users {
                    buf.extend_from_slice(u.as_bytes());
                    buf.push(b'@');
                    buf.extend_from_slice(d);
                    buf.push(b'\n');
                }
            }
        }
        OutputFormat::Csv => {
            for d in slices {
                for u in users {
                    let ub = u.as_bytes();
                    buf.extend_from_slice(ub);
                    buf.push(b',');
                    buf.extend_from_slice(d);
                    buf.push(b',');
                    buf.extend_from_slice(ub);
                    buf.push(b'@');
                    buf.extend_from_slice(d);
                    buf.push(b'\n');
                }
            }
        }
        OutputFormat::Json => {
            for d in slices {
                for u in users {
                    let ub = u.as_bytes();
                    buf.extend_from_slice(br#"{"u":""#);
                    buf.extend_from_slice(ub);
                    buf.extend_from_slice(br#"","d":""#);
                    buf.extend_from_slice(d);
                    buf.extend_from_slice(br#"","e":""#);
                    buf.extend_from_slice(ub);
                    buf.push(b'@');
                    buf.extend_from_slice(d);
                    buf.extend_from_slice(b"\"}\n");
                }
            }
        }
    }
    buf
}

/// Generate header bytes cho CSV (một lần). Plain và JSON không có header.
pub(crate) fn header_bytes(fmt: OutputFormat) -> Option<Vec<u8>> {
    match fmt {
        OutputFormat::Csv => Some(b"username,domain,email\n".to_vec()),
        _ => None,
    }
}

/// Run generator. domains được chia thành chunks, shuffle nếu cần, xử lý parallel.
/// Trả tổng số email sinh ra.
pub fn run<'a>(
    domains: &'a [&'a [u8]],
    usernames: Arc<Vec<String>>,
    cfg: &Config,
    sink: Sender<Vec<u8>>,
    progress: Option<ProgressBar>,
) -> Result<u64, GenError> {
    if usernames.is_empty() {
        return Err(GenError::UsernameEmpty);
    }
    if domains.is_empty() {
        return Err(GenError::DomainEmpty);
    }

    // Gửi CSV header trước nếu cần.
    if let Some(header) = header_bytes(cfg.format) {
        sink.send(header).map_err(|_| GenError::SendFailed)?;
    }

    // Chia domains thành chunks. Chunks là Vec<&[&[u8]]> — ref slice.
    let chunks: Vec<&[&[u8]]> = domains.chunks(cfg.chunk_size).collect();
    let mut chunk_indices: Vec<usize> = (0..chunks.len()).collect();
    if cfg.shuffle {
        chunk_indices.shuffle(&mut thread_rng());
    }

    // Byte budget — 256 MB tổng in-flight.
    let budget = Arc::new(ByteBudget::new(256 * 1024 * 1024));
    let avg_bytes = avg_bytes_per_email(&usernames, domains, cfg.format);
    let email_count = Arc::new(AtomicUsize::new(0));
    let send_failed = Arc::new(AtomicUsize::new(0));

    let users_ref = &usernames;
    let fmt = cfg.format;

    chunk_indices.into_par_iter().for_each(|ci| {
        if send_failed.load(Ordering::Relaxed) > 0 {
            return;
        }
        let chunk = chunks[ci];
        let est = chunk.len() * users_ref.len() * avg_bytes.max(1);
        budget.acquire(est);
        let buf = build_chunk(chunk, users_ref, fmt, est);
        let actual = buf.len();
        // Nếu estimate lệch, điều chỉnh budget (giải phóng phần thừa hoặc chiếm thêm).
        if actual > est {
            budget.acquire(actual - est);
        } else if actual < est {
            budget.release(est - actual);
        }
        let n_emails = (chunk.len() * users_ref.len()) as u64;
        match sink.send(buf) {
            Ok(_) => {
                email_count.fetch_add(n_emails as usize, Ordering::Relaxed);
                if let Some(pb) = progress.as_ref() {
                    pb.inc(chunk.len() as u64);
                }
                // Writer sẽ "release" budget khi consume — nhưng writer không biết byte budget.
                // Giải pháp: release ngay sau khi send thành công; budget chỉ giới hạn
                // tổng bytes đang build + chưa send. Writer có BufWriter riêng nên đây là
                // soft backpressure — kết hợp với channel bounded(16) đủ cap hard.
                budget.release(actual);
            }
            Err(_) => {
                budget.release(actual);
                send_failed.fetch_add(1, Ordering::Relaxed);
            }
        }
    });

    if send_failed.load(Ordering::Relaxed) > 0 {
        return Err(GenError::SendFailed);
    }

    Ok(email_count.load(Ordering::Relaxed) as u64)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crossbeam_channel::bounded;

    #[test]
    fn build_chunk_plain_1x1() {
        let users = vec!["alice".to_string()];
        let domains: Vec<&[u8]> = vec![b"example.com"];
        let est = avg_bytes_per_email(&users, &domains, OutputFormat::Plain);
        let buf = build_chunk(&domains, &users, OutputFormat::Plain, est);
        assert_eq!(buf, b"alice@example.com\n");
    }

    #[test]
    fn build_chunk_plain_3x2() {
        let users = vec!["a".to_string(), "bb".to_string()];
        let domains: Vec<&[u8]> = vec![b"x.com", b"y.com", b"z.com"];
        let est = 3 * 2 * avg_bytes_per_email(&users, &domains, OutputFormat::Plain);
        let buf = build_chunk(&domains, &users, OutputFormat::Plain, est);
        let s = std::str::from_utf8(&buf).unwrap();
        let lines: Vec<&str> = s.lines().collect();
        assert_eq!(lines.len(), 6);
        assert_eq!(lines[0], "a@x.com");
        assert_eq!(lines[1], "bb@x.com");
        assert_eq!(lines[5], "bb@z.com");
    }

    #[test]
    fn build_chunk_csv() {
        let users = vec!["alice".to_string()];
        let domains: Vec<&[u8]> = vec![b"example.com"];
        let buf = build_chunk(&domains, &users, OutputFormat::Csv, 64);
        let s = std::str::from_utf8(&buf).unwrap();
        assert_eq!(s, "alice,example.com,alice@example.com\n");
    }

    #[test]
    fn build_chunk_json() {
        let users = vec!["alice".to_string()];
        let domains: Vec<&[u8]> = vec![b"ex.com"];
        let buf = build_chunk(&domains, &users, OutputFormat::Json, 64);
        let s = std::str::from_utf8(&buf).unwrap();
        assert_eq!(s, "{\"u\":\"alice\",\"d\":\"ex.com\",\"e\":\"alice@ex.com\"}\n");
    }

    #[test]
    fn build_chunk_capacity_estimate_accurate() {
        let users: Vec<String> = (0..10).map(|i| format!("user{i}")).collect();
        let domain_strs: Vec<String> = (0..20).map(|i| format!("domain{i}.com")).collect();
        let domains: Vec<&[u8]> = domain_strs.iter().map(|s| s.as_bytes()).collect();
        let est = 20 * 10 * avg_bytes_per_email(&users, &domains, OutputFormat::Plain);
        let buf = build_chunk(&domains, &users, OutputFormat::Plain, est);
        // Capacity sai lệch < 10% so với thực tế
        let ratio = buf.len() as f64 / est as f64;
        assert!(
            ratio > 0.9 && ratio < 1.1,
            "est {} vs actual {} ratio {}",
            est,
            buf.len(),
            ratio
        );
    }

    #[test]
    fn header_bytes_csv_only() {
        assert!(header_bytes(OutputFormat::Plain).is_none());
        assert_eq!(
            header_bytes(OutputFormat::Csv).unwrap(),
            b"username,domain,email\n"
        );
        assert!(header_bytes(OutputFormat::Json).is_none());
    }

    #[test]
    fn byte_budget_acquire_release() {
        let budget = ByteBudget::new(1000);
        budget.acquire(500);
        assert_eq!(budget.current(), 500);
        budget.release(200);
        assert_eq!(budget.current(), 300);
    }

    #[test]
    fn byte_budget_allows_single_oversize() {
        // Khi queued=0, một request lớn hơn max vẫn đi qua (tránh deadlock).
        let budget = ByteBudget::new(100);
        budget.acquire(500);
        assert_eq!(budget.current(), 500);
    }

    fn cfg_for_test() -> Config {
        use std::path::PathBuf;
        Config {
            domains_path: PathBuf::from("d"),
            usernames_path: PathBuf::from("u"),
            output_path: PathBuf::from("o"),
            chunk_size: 2,
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

    #[test]
    fn run_generates_all_emails() {
        let users = Arc::new(vec!["a".to_string(), "b".to_string()]);
        let domain_strs: Vec<&[u8]> = vec![b"x.com", b"y.com", b"z.com"];
        let cfg = cfg_for_test();
        let (tx, rx) = bounded::<Vec<u8>>(8);
        let total = run(&domain_strs, users, &cfg, tx, None).unwrap();
        assert_eq!(total, 6); // 3 domains × 2 users
        let mut all_bytes = Vec::new();
        for buf in rx.iter() {
            all_bytes.extend(buf);
        }
        let s = std::str::from_utf8(&all_bytes).unwrap();
        let lines: Vec<&str> = s.lines().collect();
        assert_eq!(lines.len(), 6);
        // Check set content (order có thể khác vì parallel)
        let mut sorted: Vec<&str> = lines.clone();
        sorted.sort();
        assert_eq!(
            sorted,
            vec!["a@x.com", "a@y.com", "a@z.com", "b@x.com", "b@y.com", "b@z.com"]
        );
    }

    #[test]
    fn run_empty_usernames_errors() {
        let users = Arc::new(vec![]);
        let domains: Vec<&[u8]> = vec![b"x.com"];
        let cfg = cfg_for_test();
        let (tx, _rx) = bounded::<Vec<u8>>(1);
        let err = run(&domains, users, &cfg, tx, None).unwrap_err();
        assert!(matches!(err, GenError::UsernameEmpty));
    }

    #[test]
    fn run_empty_domains_errors() {
        let users = Arc::new(vec!["alice".to_string()]);
        let domains: Vec<&[u8]> = vec![];
        let cfg = cfg_for_test();
        let (tx, _rx) = bounded::<Vec<u8>>(1);
        let err = run(&domains, users, &cfg, tx, None).unwrap_err();
        assert!(matches!(err, GenError::DomainEmpty));
    }

    #[test]
    fn run_send_failed_when_rx_dropped() {
        let users = Arc::new(vec!["a".to_string()]);
        let domain_strs: Vec<&[u8]> = vec![b"x.com", b"y.com"];
        let cfg = cfg_for_test();
        let (tx, rx) = bounded::<Vec<u8>>(1);
        drop(rx);
        let err = run(&domain_strs, users, &cfg, tx, None).unwrap_err();
        assert!(matches!(err, GenError::SendFailed));
    }
}
