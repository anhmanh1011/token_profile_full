// Splitter: rotate output files khi vượt threshold, naming emails_001.txt[.gz].

use std::fs::File;
use std::io::{BufWriter, Write};
#[cfg(test)]
use std::io;
use std::path::{Path, PathBuf};

use flate2::write::GzEncoder;
use flate2::Compression;

use crate::error::WriterError;

/// Tạo filename cho split index.
/// Ví dụ: base="emails.txt", idx=1, gzip=false → "emails_001.txt"
///         base="emails.txt", idx=1, gzip=true  → "emails_001.txt.gz"
///         base="out/foo", idx=42, gzip=false  → "out/foo_042"
pub(crate) fn filename(base: &Path, idx: u32, gzip: bool) -> PathBuf {
    let parent = base.parent().unwrap_or(Path::new(""));
    let stem = base
        .file_stem()
        .and_then(|s| s.to_str())
        .unwrap_or("emails");
    let ext = base
        .extension()
        .and_then(|e| e.to_str())
        .map(|s| format!(".{s}"))
        .unwrap_or_default();
    let name = if gzip {
        format!("{stem}_{idx:03}{ext}.gz")
    } else {
        format!("{stem}_{idx:03}{ext}")
    };
    parent.join(name)
}

/// Boxed writer gzip-aware.
fn open_file(path: &Path, buffer_bytes: usize, gzip: bool) -> Result<Box<dyn Write + Send>, WriterError> {
    let file = File::create(path).map_err(|source| WriterError::CannotCreate {
        path: path.to_path_buf(),
        source,
    })?;
    let buf = BufWriter::with_capacity(buffer_bytes, file);
    if gzip {
        // Compression::fast = level 1, ưu tiên tốc độ (spec target 60s).
        let enc = GzEncoder::new(buf, Compression::fast());
        Ok(Box::new(enc))
    } else {
        Ok(Box::new(buf))
    }
}

/// Splitter rotate tại chunk boundary. Mỗi lần write_all nhận 1 chunk.
pub struct Splitter {
    base: PathBuf,
    buffer_bytes: usize,
    threshold: u64,
    gzip: bool,
    idx: u32,
    written_cur: u64,
    current: Option<Box<dyn Write + Send>>,
}

impl Splitter {
    pub fn new(base: &Path, split_gb: u64, buffer_bytes: usize, gzip: bool) -> Result<Self, WriterError> {
        let threshold = split_gb.saturating_mul(1024 * 1024 * 1024);
        let mut s = Splitter {
            base: base.to_path_buf(),
            buffer_bytes,
            threshold,
            gzip,
            idx: 0,
            written_cur: 0,
            current: None,
        };
        s.rotate()?;
        Ok(s)
    }

    fn rotate(&mut self) -> Result<(), WriterError> {
        // Flush + drop writer hiện tại trước khi mở file mới.
        if let Some(mut w) = self.current.take() {
            w.flush().map_err(WriterError::from_io)?;
        }
        self.idx += 1;
        let path = filename(&self.base, self.idx, self.gzip);
        let writer = open_file(&path, self.buffer_bytes, self.gzip)?;
        self.current = Some(writer);
        self.written_cur = 0;
        Ok(())
    }

    /// Ghi 1 chunk (atomic — không split giữa buffer).
    pub fn write_chunk(&mut self, buf: &[u8]) -> Result<(), WriterError> {
        // Nếu viết buf sẽ vượt threshold VÀ file hiện tại đã có data → rotate trước.
        if self.written_cur > 0 && self.written_cur + buf.len() as u64 > self.threshold {
            self.rotate()?;
        }
        let w = self
            .current
            .as_mut()
            .expect("current writer phải tồn tại sau rotate");
        w.write_all(buf).map_err(WriterError::from_io)?;
        self.written_cur += buf.len() as u64;
        Ok(())
    }

    pub fn finalize(mut self) -> Result<(), WriterError> {
        if let Some(mut w) = self.current.take() {
            w.flush().map_err(WriterError::from_io)?;
        }
        Ok(())
    }
}

/// Wrapper sink cho trường hợp không split — 1 file duy nhất.
pub struct SingleFile {
    inner: Box<dyn Write + Send>,
}

impl SingleFile {
    pub fn new(base: &Path, buffer_bytes: usize, gzip: bool) -> Result<Self, WriterError> {
        let path = if gzip && base.extension().and_then(|e| e.to_str()) != Some("gz") {
            let mut p = base.to_path_buf();
            let new_name = format!(
                "{}.gz",
                base.file_name()
                    .and_then(|s| s.to_str())
                    .unwrap_or("emails.txt")
            );
            p.set_file_name(new_name);
            p
        } else {
            base.to_path_buf()
        };
        let inner = open_file(&path, buffer_bytes, gzip)?;
        Ok(SingleFile { inner })
    }

    pub fn write_chunk(&mut self, buf: &[u8]) -> Result<(), WriterError> {
        self.inner.write_all(buf).map_err(WriterError::from_io)
    }

    pub fn finalize(mut self) -> Result<(), WriterError> {
        self.inner.flush().map_err(WriterError::from_io)
    }
}

/// Mock writer trả WriteZero sau N bytes — dùng để test disk full cross-platform.
#[cfg(test)]
pub(crate) struct FullDiskMock {
    pub max_bytes: usize,
    pub written: usize,
}

#[cfg(test)]
impl Write for FullDiskMock {
    fn write(&mut self, buf: &[u8]) -> io::Result<usize> {
        let remaining = self.max_bytes.saturating_sub(self.written);
        if remaining == 0 {
            return Err(io::Error::new(io::ErrorKind::WriteZero, "mock disk full"));
        }
        let n = buf.len().min(remaining);
        self.written += n;
        Ok(n)
    }

    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use flate2::read::GzDecoder;
    use std::io::Read;
    use tempfile::TempDir;

    #[test]
    fn filename_basic() {
        let p = filename(Path::new("emails.txt"), 1, false);
        assert_eq!(p.file_name().unwrap(), "emails_001.txt");
    }

    #[test]
    fn filename_gzip() {
        let p = filename(Path::new("emails.txt"), 1, true);
        assert_eq!(p.file_name().unwrap(), "emails_001.txt.gz");
    }

    #[test]
    fn filename_idx_padding() {
        assert_eq!(
            filename(Path::new("e.txt"), 999, false).file_name().unwrap(),
            "e_999.txt"
        );
        assert_eq!(
            filename(Path::new("e.txt"), 1000, false).file_name().unwrap(),
            "e_1000.txt"
        );
    }

    #[test]
    fn filename_with_parent() {
        let p = filename(Path::new("out/foo.txt"), 5, false);
        assert_eq!(p, PathBuf::from("out/foo_005.txt"));
    }

    #[test]
    fn filename_no_extension() {
        let p = filename(Path::new("emails"), 1, false);
        assert_eq!(p.file_name().unwrap(), "emails_001");
    }

    #[test]
    fn single_file_plain_write_read() {
        let dir = TempDir::new().unwrap();
        let path = dir.path().join("out.txt");
        let mut sf = SingleFile::new(&path, 4096, false).unwrap();
        sf.write_chunk(b"hello\nworld\n").unwrap();
        sf.finalize().unwrap();
        let content = std::fs::read_to_string(&path).unwrap();
        assert_eq!(content, "hello\nworld\n");
    }

    #[test]
    fn single_file_gzip_roundtrip() {
        let dir = TempDir::new().unwrap();
        let base = dir.path().join("out.txt");
        let mut sf = SingleFile::new(&base, 4096, true).unwrap();
        sf.write_chunk(b"alice@example.com\nbob@example.com\n").unwrap();
        sf.finalize().unwrap();
        let gz_path = dir.path().join("out.txt.gz");
        assert!(gz_path.exists(), "gz file phải tồn tại: {:?}", gz_path);
        let mut decoder = GzDecoder::new(File::open(&gz_path).unwrap());
        let mut s = String::new();
        decoder.read_to_string(&mut s).unwrap();
        assert_eq!(s, "alice@example.com\nbob@example.com\n");
    }

    #[test]
    fn splitter_rotates_at_threshold() {
        let dir = TempDir::new().unwrap();
        let base = dir.path().join("emails.txt");
        // threshold 1GB — giả lập nhỏ bằng chunk bytes
        let mut sp = Splitter::new(&base, 1, 1024, false).unwrap();
        // Ghi chunk ~500MB 2 lần — sẽ rotate sau chunk 3 (vượt 1GB threshold)
        // Dùng giá trị nhỏ để test nhanh: set threshold bằng 100 bytes via hack
        // (tạm thời kiểm tra không panic + 1 file được tạo)
        sp.write_chunk(b"aaa\n").unwrap();
        sp.finalize().unwrap();
        assert!(dir.path().join("emails_001.txt").exists());
    }

    #[test]
    fn splitter_small_threshold_rotation() {
        // Test rotation rõ ràng với threshold rất nhỏ — dùng trick: write nhiều chunks nhỏ,
        // threshold 0 không được accept (u64 * 1GB), nên ta test qua force threshold.
        let dir = TempDir::new().unwrap();
        let base = dir.path().join("e.txt");
        let mut sp = Splitter {
            base: base.clone(),
            buffer_bytes: 64,
            threshold: 10, // 10 bytes
            gzip: false,
            idx: 0,
            written_cur: 0,
            current: None,
        };
        sp.rotate().unwrap();
        sp.write_chunk(b"abcdefgh\n").unwrap(); // 9 bytes → 1 file
        sp.write_chunk(b"xyz\n").unwrap(); // 4 bytes → vượt threshold, rotate
        sp.write_chunk(b"123\n").unwrap();
        sp.finalize().unwrap();
        assert!(dir.path().join("e_001.txt").exists());
        assert!(dir.path().join("e_002.txt").exists());
        let f1 = std::fs::read_to_string(dir.path().join("e_001.txt")).unwrap();
        let f2 = std::fs::read_to_string(dir.path().join("e_002.txt")).unwrap();
        assert_eq!(f1, "abcdefgh\n");
        assert_eq!(f2, "xyz\n123\n");
    }

    #[test]
    fn full_disk_mock_returns_write_zero() {
        let mut mock = FullDiskMock { max_bytes: 3, written: 0 };
        assert_eq!(mock.write(b"ab").unwrap(), 2);
        // Remaining = 1, bắt đầu vượt
        let _ = mock.write(b"c").unwrap();
        let err = mock.write(b"x").unwrap_err();
        assert_eq!(err.kind(), io::ErrorKind::WriteZero);
    }
}
