// Stats struct + pretty print + cross-platform RAM peak.

use std::path::{Path, PathBuf};
use std::time::Duration;

#[derive(Debug, Clone)]
pub struct Stats {
    pub domains: u64,
    pub usernames: u64,
    pub emails: u64,
    pub output: PathBuf,
    pub file_size: u64,
    pub elapsed: Duration,
    pub threads: usize,
    pub ram_peak_mb: u64,
}

impl Stats {
    pub fn throughput_mb_s(&self) -> f64 {
        let secs = self.elapsed.as_secs_f64();
        if secs <= 0.0 {
            return 0.0;
        }
        (self.file_size as f64) / (1024.0 * 1024.0) / secs
    }

    pub fn print_vi(&self) {
        let size_gb = (self.file_size as f64) / (1024.0 * 1024.0 * 1024.0);
        println!("✅ Done!");
        println!("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━");
        println!("📊 Domains processed : {}", fmt_num(self.domains));
        println!("👥 Usernames         : {}", fmt_num(self.usernames));
        println!("📧 Total emails      : {}", fmt_num(self.emails));
        println!("📁 Output file       : {}", self.output.display());
        println!("💾 File size         : {:.2} GB", size_gb);
        println!("⏱  Time              : {:.1}s", self.elapsed.as_secs_f64());
        println!("🚀 Throughput        : {:.0} MB/s", self.throughput_mb_s());
        println!("🧵 Worker threads    : {}", self.threads);
        println!("🔋 RAM peak          : {} MB", self.ram_peak_mb);
        println!("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━");
    }
}

fn fmt_num(n: u64) -> String {
    let s = n.to_string();
    let bytes = s.as_bytes();
    let mut out = String::with_capacity(s.len() + s.len() / 3);
    for (i, b) in bytes.iter().enumerate() {
        if i > 0 && (bytes.len() - i).is_multiple_of(3) {
            out.push(',');
        }
        out.push(*b as char);
    }
    out
}

/// File size trên disk. Hỗ trợ file thường và split files dạng {base}_NNN.{ext}.
pub fn output_total_size(path: &Path, split: bool) -> u64 {
    if !split {
        return std::fs::metadata(path).map(|m| m.len()).unwrap_or(0);
    }
    // Scan thư mục cha tìm file khớp prefix {stem}_
    let Some(parent) = path.parent() else { return 0; };
    let Some(stem) = path.file_stem().and_then(|s| s.to_str()) else { return 0; };
    let parent = if parent.as_os_str().is_empty() {
        Path::new(".")
    } else {
        parent
    };
    let Ok(entries) = std::fs::read_dir(parent) else { return 0; };
    let mut total = 0u64;
    for entry in entries.flatten() {
        if let Some(name) = entry.file_name().to_str() {
            if name.starts_with(&format!("{stem}_")) {
                if let Ok(meta) = entry.metadata() {
                    total += meta.len();
                }
            }
        }
    }
    total
}

/// RAM peak MB (cross-platform). Trả về 0 nếu không đọc được.
pub fn ram_peak_mb() -> u64 {
    platform::ram_peak_mb()
}

#[cfg(target_os = "linux")]
mod platform {
    pub fn ram_peak_mb() -> u64 {
        // /proc/self/status chứa dòng "VmHWM: <kb> kB".
        let Ok(s) = std::fs::read_to_string("/proc/self/status") else {
            return 0;
        };
        for line in s.lines() {
            if let Some(rest) = line.strip_prefix("VmHWM:") {
                let kb: u64 = rest
                    .split_whitespace()
                    .next()
                    .and_then(|s| s.parse().ok())
                    .unwrap_or(0);
                return kb / 1024;
            }
        }
        0
    }
}

#[cfg(target_os = "windows")]
mod platform {
    // Gọi GetProcessMemoryInfo qua psapi. Declare bindings inline để tránh thêm deps.
    use std::mem::{size_of, zeroed};

    #[repr(C)]
    struct ProcessMemoryCounters {
        cb: u32,
        page_fault_count: u32,
        peak_working_set_size: usize,
        working_set_size: usize,
        quota_peak_paged_pool_usage: usize,
        quota_paged_pool_usage: usize,
        quota_peak_non_paged_pool_usage: usize,
        quota_non_paged_pool_usage: usize,
        pagefile_usage: usize,
        peak_pagefile_usage: usize,
    }

    #[allow(clippy::upper_case_acronyms)]
    type HANDLE = *mut std::ffi::c_void;

    extern "system" {
        fn GetCurrentProcess() -> HANDLE;
        fn GetProcessMemoryInfo(
            process: HANDLE,
            counters: *mut ProcessMemoryCounters,
            cb: u32,
        ) -> i32;
    }

    pub fn ram_peak_mb() -> u64 {
        unsafe {
            let mut counters: ProcessMemoryCounters = zeroed();
            counters.cb = size_of::<ProcessMemoryCounters>() as u32;
            let ok = GetProcessMemoryInfo(
                GetCurrentProcess(),
                &mut counters,
                counters.cb,
            );
            if ok == 0 {
                return 0;
            }
            (counters.peak_working_set_size as u64) / (1024 * 1024)
        }
    }
}

#[cfg(not(any(target_os = "linux", target_os = "windows")))]
mod platform {
    pub fn ram_peak_mb() -> u64 {
        0
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn throughput_zero_duration_safe() {
        let s = Stats {
            domains: 0,
            usernames: 0,
            emails: 0,
            output: PathBuf::from("x"),
            file_size: 1024,
            elapsed: Duration::from_secs(0),
            threads: 1,
            ram_peak_mb: 0,
        };
        assert_eq!(s.throughput_mb_s(), 0.0);
    }

    #[test]
    fn throughput_basic() {
        let s = Stats {
            domains: 0,
            usernames: 0,
            emails: 0,
            output: PathBuf::from("x"),
            file_size: 1024 * 1024 * 10,
            elapsed: Duration::from_secs(2),
            threads: 1,
            ram_peak_mb: 0,
        };
        assert!((s.throughput_mb_s() - 5.0).abs() < 0.01);
    }

    #[test]
    fn fmt_num_thousand_separator() {
        assert_eq!(fmt_num(0), "0");
        assert_eq!(fmt_num(123), "123");
        assert_eq!(fmt_num(1_234), "1,234");
        assert_eq!(fmt_num(200_000_000), "200,000,000");
    }

    #[test]
    fn ram_peak_returns_non_negative() {
        // Không thể assert giá trị cụ thể (platform-dependent), chỉ chắc chắn không panic.
        let _ = ram_peak_mb();
    }
}
