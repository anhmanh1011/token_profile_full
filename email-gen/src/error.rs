// Định nghĩa các error enum cho từng module.
// Dùng thiserror cho typed errors, main.rs sẽ dùng anyhow để compose.

use std::io;
use std::path::PathBuf;

use thiserror::Error;

#[derive(Debug, Error)]
pub enum ConfigError {
    #[error("chunk_size phải > 0")]
    InvalidChunkSize,

    #[error("threads phải > 0")]
    InvalidThreads,

    #[error("buffer_size phải > 0 MB")]
    InvalidBufferSize,

    #[error("format không hợp lệ: {0} (chấp nhận: plain|csv|json)")]
    InvalidFormat(String),

    #[error("các flag xung đột: {0}")]
    ConflictingFlags(String),

    #[error("overflow: domains ({domains}) x usernames ({usernames}) vượt giới hạn u64")]
    Overflow { domains: u64, usernames: u64 },
}

#[derive(Debug, Error)]
pub enum ReaderError {
    #[error("file không tồn tại: {0}")]
    NotFound(PathBuf),

    #[error("không có quyền đọc file: {0}")]
    PermissionDenied(PathBuf),

    #[error("mmap thất bại cho {path}: {source}")]
    MmapFailed {
        path: PathBuf,
        #[source]
        source: io::Error,
    },

    #[error("file rỗng: {0}")]
    EmptyFile(PathBuf),

    #[error("ký tự UTF-8 không hợp lệ ở dòng {line} trong {path}")]
    InvalidUtf8 { path: PathBuf, line: u32 },

    #[error("I/O error đọc {path}: {source}")]
    Io {
        path: PathBuf,
        #[source]
        source: io::Error,
    },
}

impl ReaderError {
    pub fn from_io(path: &std::path::Path, source: io::Error) -> Self {
        match source.kind() {
            io::ErrorKind::NotFound => ReaderError::NotFound(path.to_path_buf()),
            io::ErrorKind::PermissionDenied => ReaderError::PermissionDenied(path.to_path_buf()),
            _ => ReaderError::Io {
                path: path.to_path_buf(),
                source,
            },
        }
    }
}

#[derive(Debug, Error)]
pub enum GenError {
    #[error("kênh đã đóng (writer thread chết?)")]
    SendFailed,

    #[error("usernames.txt rỗng — cần ít nhất 1 username")]
    UsernameEmpty,

    #[error("domains.txt rỗng sau khi lọc — cần ít nhất 1 domain hợp lệ")]
    DomainEmpty,
}

#[derive(Debug, Error)]
pub enum WriterError {
    #[error("ổ đĩa đầy")]
    DiskFull,

    #[error("gzip encoder lỗi: {0}")]
    GzipFailed(#[source] io::Error),

    #[error("I/O error: {0}")]
    Io(#[from] io::Error),

    #[error("không thể tạo file output: {path}")]
    CannotCreate {
        path: PathBuf,
        #[source]
        source: io::Error,
    },
}

impl WriterError {
    // Windows: ERROR_DISK_FULL = 112; Linux: ENOSPC = 28.
    pub fn from_io(source: io::Error) -> Self {
        if let Some(code) = source.raw_os_error() {
            if code == 28 || code == 112 {
                return WriterError::DiskFull;
            }
        }
        // WriteZero là tín hiệu /dev/full hoặc mock — coi như disk full.
        if source.kind() == io::ErrorKind::WriteZero {
            return WriterError::DiskFull;
        }
        WriterError::Io(source)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn writer_error_disk_full_linux() {
        let err = io::Error::from_raw_os_error(28);
        assert!(matches!(WriterError::from_io(err), WriterError::DiskFull));
    }

    #[test]
    fn writer_error_disk_full_windows() {
        let err = io::Error::from_raw_os_error(112);
        assert!(matches!(WriterError::from_io(err), WriterError::DiskFull));
    }

    #[test]
    fn writer_error_write_zero_maps_to_disk_full() {
        let err = io::Error::new(io::ErrorKind::WriteZero, "mock");
        assert!(matches!(WriterError::from_io(err), WriterError::DiskFull));
    }

    #[test]
    fn reader_error_not_found_mapped() {
        let err = io::Error::new(io::ErrorKind::NotFound, "x");
        let mapped = ReaderError::from_io(std::path::Path::new("/tmp/nope"), err);
        assert!(matches!(mapped, ReaderError::NotFound(_)));
    }
}
