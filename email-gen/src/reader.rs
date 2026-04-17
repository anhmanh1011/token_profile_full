// Mmap-based reader: zero-copy, dùng memchr để scan newline nhanh.

use std::collections::HashSet;
use std::fs::File;
use std::path::Path;
use std::sync::Arc;

use memmap2::Mmap;

use crate::error::ReaderError;

/// Wrapper ownership cho mmap — giữ File sống cùng Mmap.
#[derive(Debug)]
pub struct MappedFile {
    pub mmap: Mmap,
    _file: File,
}

impl MappedFile {
    pub fn as_bytes(&self) -> &[u8] {
        &self.mmap
    }
}

/// Mở file và mmap read-only.
pub fn map(path: &Path) -> Result<MappedFile, ReaderError> {
    let file = File::open(path).map_err(|e| ReaderError::from_io(path, e))?;
    let meta = file
        .metadata()
        .map_err(|e| ReaderError::from_io(path, e))?;
    if meta.len() == 0 {
        return Err(ReaderError::EmptyFile(path.to_path_buf()));
    }
    // SAFETY: mmap yêu cầu caller không modify file trong suốt thời gian map.
    // Đây là batch job read-only, không ai ghi đồng thời.
    let mmap = unsafe { Mmap::map(&file) }.map_err(|e| ReaderError::MmapFailed {
        path: path.to_path_buf(),
        source: e,
    })?;
    Ok(MappedFile { mmap, _file: file })
}

/// Scan buffer tìm line boundaries. Trả Vec<(start, end)> — end không bao gồm \n/\r.
/// Bỏ qua dòng trắng (sau trim trailing \r). Dùng u32 để tiết kiệm 50% memory vs usize.
pub fn line_offsets(buf: &[u8]) -> Vec<(u32, u32)> {
    let mut offsets = Vec::with_capacity(buf.len() / 32); // heuristic
    let mut start: usize = 0;
    for pos in memchr::memchr_iter(b'\n', buf) {
        let mut end = pos;
        // Trim \r trước \n (CRLF)
        if end > start && buf[end - 1] == b'\r' {
            end -= 1;
        }
        // Trim whitespace đầu/cuối (space, tab)
        let (s, e) = trim_ws(buf, start, end);
        if e > s {
            offsets.push((s as u32, e as u32));
        }
        start = pos + 1;
    }
    // Xử lý line cuối không có \n
    if start < buf.len() {
        let mut end = buf.len();
        if end > start && buf[end - 1] == b'\r' {
            end -= 1;
        }
        let (s, e) = trim_ws(buf, start, end);
        if e > s {
            offsets.push((s as u32, e as u32));
        }
    }
    offsets
}

#[inline]
fn trim_ws(buf: &[u8], mut start: usize, mut end: usize) -> (usize, usize) {
    while start < end && matches!(buf[start], b' ' | b'\t') {
        start += 1;
    }
    while end > start && matches!(buf[end - 1], b' ' | b'\t') {
        end -= 1;
    }
    (start, end)
}

/// Biến offsets thành slices. Nếu dedup=true, chỉ giữ domain duy nhất (preserve order đầu).
pub fn domain_slices<'a>(
    buf: &'a [u8],
    offsets: &[(u32, u32)],
    dedup: bool,
) -> Vec<&'a [u8]> {
    if !dedup {
        return offsets
            .iter()
            .map(|&(s, e)| &buf[s as usize..e as usize])
            .collect();
    }
    let mut seen: HashSet<&[u8]> = HashSet::with_capacity(offsets.len());
    let mut out = Vec::with_capacity(offsets.len());
    for &(s, e) in offsets {
        let slice = &buf[s as usize..e as usize];
        if seen.insert(slice) {
            out.push(slice);
        }
    }
    out
}

/// Load usernames.txt vào Arc<Vec<String>>. Validate UTF-8 chi tiết theo line.
pub fn load_usernames(path: &Path) -> Result<Arc<Vec<String>>, ReaderError> {
    let mapped = map(path)?;
    let buf = mapped.as_bytes();
    let offsets = line_offsets(buf);
    let mut users = Vec::with_capacity(offsets.len());
    for (i, &(s, e)) in offsets.iter().enumerate() {
        let slice = &buf[s as usize..e as usize];
        let s_str = std::str::from_utf8(slice).map_err(|_| ReaderError::InvalidUtf8 {
            path: path.to_path_buf(),
            line: (i + 1) as u32,
        })?;
        users.push(s_str.to_string());
    }
    Ok(Arc::new(users))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use tempfile::NamedTempFile;

    fn write_tmp(content: &[u8]) -> NamedTempFile {
        let mut f = NamedTempFile::new().unwrap();
        f.write_all(content).unwrap();
        f.flush().unwrap();
        f
    }

    #[test]
    fn line_offsets_empty() {
        assert_eq!(line_offsets(b""), vec![]);
    }

    #[test]
    fn line_offsets_single_no_newline() {
        assert_eq!(line_offsets(b"hello"), vec![(0, 5)]);
    }

    #[test]
    fn line_offsets_lf() {
        assert_eq!(
            line_offsets(b"a\nbb\nccc"),
            vec![(0, 1), (2, 4), (5, 8)]
        );
    }

    #[test]
    fn line_offsets_crlf() {
        assert_eq!(
            line_offsets(b"a\r\nbb\r\nccc\r\n"),
            vec![(0, 1), (3, 5), (7, 10)]
        );
    }

    #[test]
    fn line_offsets_skip_blanks() {
        assert_eq!(
            line_offsets(b"a\n\n  \nb\n"),
            vec![(0, 1), (6, 7)]
        );
    }

    #[test]
    fn line_offsets_trim_whitespace() {
        assert_eq!(line_offsets(b"  hello  \n"), vec![(2, 7)]);
        assert_eq!(line_offsets(b"\tfoo\t\n"), vec![(1, 4)]);
    }

    #[test]
    fn domain_slices_no_dedup() {
        let buf: &[u8] = b"a.com\nb.com\na.com\n";
        let offs = line_offsets(buf);
        let slices = domain_slices(buf, &offs, false);
        assert_eq!(slices.len(), 3);
    }

    #[test]
    fn domain_slices_dedup_preserves_order() {
        let buf: &[u8] = b"a.com\nb.com\na.com\nc.com\nb.com\n";
        let offs = line_offsets(buf);
        let slices = domain_slices(buf, &offs, true);
        assert_eq!(slices.len(), 3);
        assert_eq!(slices[0], b"a.com");
        assert_eq!(slices[1], b"b.com");
        assert_eq!(slices[2], b"c.com");
    }

    #[test]
    fn domain_slices_dedup_preserves_case() {
        let buf: &[u8] = b"a.COM\na.com\n";
        let offs = line_offsets(buf);
        let slices = domain_slices(buf, &offs, true);
        assert_eq!(slices.len(), 2); // Không lowercase
    }

    #[test]
    fn map_missing_file() {
        let err = map(Path::new("/nonexistent/xyz/abc.txt")).unwrap_err();
        assert!(matches!(err, ReaderError::NotFound(_)));
    }

    #[test]
    fn map_empty_file() {
        let f = NamedTempFile::new().unwrap();
        let err = map(f.path()).unwrap_err();
        assert!(matches!(err, ReaderError::EmptyFile(_)));
    }

    #[test]
    fn load_usernames_ok() {
        let f = write_tmp(b"alice\nbob\n  carol  \n");
        let users = load_usernames(f.path()).unwrap();
        assert_eq!(users.as_slice(), &["alice".to_string(), "bob".to_string(), "carol".to_string()]);
    }

    #[test]
    fn load_usernames_invalid_utf8() {
        // 0xFF không phải start byte UTF-8 hợp lệ
        let f = write_tmp(b"alice\n\xff\xfe\nbob\n");
        let err = load_usernames(f.path()).unwrap_err();
        assert!(matches!(err, ReaderError::InvalidUtf8 { line: 2, .. }));
    }

    #[test]
    fn load_usernames_unicode_ok() {
        let f = write_tmp("user_münchen\nuser_日本\n".as_bytes());
        let users = load_usernames(f.path()).unwrap();
        assert_eq!(users.len(), 2);
    }
}
