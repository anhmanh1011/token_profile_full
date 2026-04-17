// Dedicated IO writer thread. Nhận Vec<u8> từ crossbeam channel, ghi qua SingleFile/Splitter.

use std::path::Path;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use std::thread::{self, JoinHandle};

use crossbeam_channel::Receiver;
use indicatif::ProgressBar;

use crate::config::Config;
use crate::error::WriterError;
use crate::splitter::{SingleFile, Splitter};

pub struct WriterStats {
    pub bytes_written: u64,
}

enum Sink {
    Single(SingleFile),
    Split(Splitter),
    DryRun,
}

impl Sink {
    fn write_chunk(&mut self, buf: &[u8]) -> Result<(), WriterError> {
        match self {
            Sink::Single(s) => s.write_chunk(buf),
            Sink::Split(s) => s.write_chunk(buf),
            Sink::DryRun => Ok(()),
        }
    }

    fn finalize(self) -> Result<(), WriterError> {
        match self {
            Sink::Single(s) => s.finalize(),
            Sink::Split(s) => s.finalize(),
            Sink::DryRun => Ok(()),
        }
    }
}

fn open_sink(cfg: &Config) -> Result<Sink, WriterError> {
    if cfg.dry_run {
        return Ok(Sink::DryRun);
    }
    let buffer_bytes = cfg.buffer_mb * 1024 * 1024;
    if let Some(gb) = cfg.split_gb {
        let sp = Splitter::new(&cfg.output_path, gb, buffer_bytes, cfg.gzip)?;
        Ok(Sink::Split(sp))
    } else {
        let sf = SingleFile::new(&cfg.output_path, buffer_bytes, cfg.gzip)?;
        Ok(Sink::Single(sf))
    }
}

pub type WriterHandle = JoinHandle<Result<WriterStats, WriterError>>;

/// Spawn writer thread. Trả (handle, bytes_written_atomic).
/// Worker threads send Vec<u8> qua `rx`. Đóng tx (drop) để kết thúc.
pub fn spawn_writer(
    rx: Receiver<Vec<u8>>,
    cfg: &Config,
    progress_bytes: Option<ProgressBar>,
) -> Result<(WriterHandle, Arc<AtomicU64>), WriterError> {
    let sink = open_sink(cfg)?;
    let counter = Arc::new(AtomicU64::new(0));
    let counter_clone = counter.clone();

    let handle = thread::Builder::new()
        .name("email-gen-writer".into())
        .spawn(move || -> Result<WriterStats, WriterError> {
            let mut sink = sink;
            while let Ok(buf) = rx.recv() {
                sink.write_chunk(&buf)?;
                counter_clone.fetch_add(buf.len() as u64, Ordering::Relaxed);
                if let Some(pb) = progress_bytes.as_ref() {
                    pb.set_position(counter_clone.load(Ordering::Relaxed));
                }
            }
            sink.finalize()?;
            if let Some(pb) = progress_bytes.as_ref() {
                pb.finish_with_message("done");
            }
            Ok(WriterStats {
                bytes_written: counter_clone.load(Ordering::Relaxed),
            })
        })
        .expect("spawn writer thread");

    Ok((handle, counter))
}

/// Helper: làm bytes budget từ MB.
pub fn budget_bytes(mb: usize) -> usize {
    mb * 1024 * 1024
}

/// Tính output path thực cho single file có gzip (dùng cho stats).
pub fn effective_output_path(cfg: &Config) -> std::path::PathBuf {
    if cfg.gzip && cfg.split_gb.is_none() {
        let base: &Path = &cfg.output_path;
        let name = format!(
            "{}.gz",
            base.file_name().and_then(|s| s.to_str()).unwrap_or("emails.txt")
        );
        let mut p = base.to_path_buf();
        p.set_file_name(name);
        p
    } else {
        cfg.output_path.clone()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::config::OutputFormat;
    use crossbeam_channel::bounded;
    use std::path::PathBuf;
    use tempfile::TempDir;

    fn test_config(path: PathBuf) -> Config {
        Config {
            domains_path: PathBuf::from("d"),
            usernames_path: PathBuf::from("u"),
            output_path: path,
            chunk_size: 100,
            threads: 1,
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
    fn writer_writes_single_file() {
        let dir = TempDir::new().unwrap();
        let cfg = test_config(dir.path().join("out.txt"));
        let (tx, rx) = bounded::<Vec<u8>>(4);
        let (handle, counter) = spawn_writer(rx, &cfg, None).unwrap();
        tx.send(b"a@b.com\n".to_vec()).unwrap();
        tx.send(b"c@d.com\n".to_vec()).unwrap();
        drop(tx);
        let stats = handle.join().unwrap().unwrap();
        assert_eq!(stats.bytes_written, 16);
        assert_eq!(counter.load(Ordering::Relaxed), 16);
        let content = std::fs::read_to_string(dir.path().join("out.txt")).unwrap();
        assert_eq!(content, "a@b.com\nc@d.com\n");
    }

    #[test]
    fn writer_dry_run_no_file() {
        let dir = TempDir::new().unwrap();
        let mut cfg = test_config(dir.path().join("out.txt"));
        cfg.dry_run = true;
        let (tx, rx) = bounded::<Vec<u8>>(4);
        let (handle, _counter) = spawn_writer(rx, &cfg, None).unwrap();
        tx.send(b"nope\n".to_vec()).unwrap();
        drop(tx);
        let stats = handle.join().unwrap().unwrap();
        assert_eq!(stats.bytes_written, 5);
        assert!(!dir.path().join("out.txt").exists());
    }

    #[test]
    fn writer_gzip_file_named_correctly() {
        let dir = TempDir::new().unwrap();
        let mut cfg = test_config(dir.path().join("out.txt"));
        cfg.gzip = true;
        let (tx, rx) = bounded::<Vec<u8>>(4);
        let (handle, _counter) = spawn_writer(rx, &cfg, None).unwrap();
        tx.send(b"hello\n".to_vec()).unwrap();
        drop(tx);
        handle.join().unwrap().unwrap();
        assert!(dir.path().join("out.txt.gz").exists());
    }

    #[test]
    fn effective_output_path_adds_gz() {
        let mut cfg = test_config(PathBuf::from("emails.txt"));
        cfg.gzip = true;
        assert_eq!(effective_output_path(&cfg), PathBuf::from("emails.txt.gz"));
    }

    #[test]
    fn budget_bytes_calc() {
        assert_eq!(budget_bytes(64), 64 * 1024 * 1024);
    }
}
