// CLI parsing (clap derive) + Config validation.

use std::path::PathBuf;
use std::str::FromStr;

use clap::Parser;

use crate::error::ConfigError;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum OutputFormat {
    Plain,
    Csv,
    Json,
}

impl FromStr for OutputFormat {
    type Err = ConfigError;
    fn from_str(s: &str) -> Result<Self, Self::Err> {
        match s.to_ascii_lowercase().as_str() {
            "plain" => Ok(OutputFormat::Plain),
            "csv" => Ok(OutputFormat::Csv),
            "json" => Ok(OutputFormat::Json),
            other => Err(ConfigError::InvalidFormat(other.to_string())),
        }
    }
}

/// CLI args exposed cho user.
#[derive(Parser, Debug)]
#[command(
    name = "email-gen",
    version,
    about = "Generate cross-product emails từ domains × usernames (Rust, parallel, mmap-based)"
)]
pub struct Cli {
    /// File domains (1 domain/line)
    #[arg(short = 'd', long, default_value = "domains.txt")]
    pub domains: PathBuf,

    /// File usernames (1 username/line)
    #[arg(short = 'u', long, default_value = "usernames.txt")]
    pub usernames: PathBuf,

    /// File output
    #[arg(short = 'o', long, default_value = "emails.txt")]
    pub output: PathBuf,

    /// Số domains mỗi chunk. Default 2000 giữ RAM < 500MB với 200 usernames.
    /// Có thể tăng lên (vd -c 20000) nếu chấp nhận RAM cao hơn.
    #[arg(short = 'c', long, default_value_t = 2000)]
    pub chunk_size: usize,

    /// Số worker threads. 0 = auto (num_cpus).
    #[arg(short = 't', long, default_value_t = 0)]
    pub threads: usize,

    /// Buffer size cho BufWriter (MB)
    #[arg(short = 'b', long, default_value_t = 64)]
    pub buffer_size: usize,

    /// Split output thành nhiều file, mỗi file N GB
    #[arg(long, value_name = "SIZE_GB")]
    pub split: Option<u64>,

    /// Gzip compress output
    #[arg(long)]
    pub gzip: bool,

    /// Dedup domains (chỉ dedup input, không dedup emails).
    /// Email uniqueness được bảo đảm khi cả domains và usernames đều unique.
    #[arg(long)]
    pub dedup: bool,

    /// Shuffle thứ tự chunks được ghi ra (không shuffle trong chunk để giữ RAM bounded).
    #[arg(long)]
    pub shuffle: bool,

    /// Format output: plain | csv | json
    #[arg(long, default_value = "plain")]
    pub format: String,

    /// Hiện progress bar
    #[arg(long)]
    pub progress: bool,

    /// Chỉ tính toán, không ghi file
    #[arg(long)]
    pub dry_run: bool,

    /// Verbose logging
    #[arg(short = 'v', long)]
    pub verbose: bool,
}

#[derive(Debug, Clone)]
pub struct Config {
    pub domains_path: PathBuf,
    pub usernames_path: PathBuf,
    pub output_path: PathBuf,
    pub chunk_size: usize,
    pub threads: usize,
    pub buffer_mb: usize,
    pub split_gb: Option<u64>,
    pub gzip: bool,
    pub dedup: bool,
    pub shuffle: bool,
    pub format: OutputFormat,
    pub progress: bool,
    pub dry_run: bool,
    pub verbose: bool,
}

impl Config {
    pub fn from_cli(cli: Cli) -> Result<Self, ConfigError> {
        let threads = if cli.threads == 0 {
            num_cpus::get()
        } else {
            cli.threads
        };
        let format = OutputFormat::from_str(&cli.format)?;
        let cfg = Config {
            domains_path: cli.domains,
            usernames_path: cli.usernames,
            output_path: cli.output,
            chunk_size: cli.chunk_size,
            threads,
            buffer_mb: cli.buffer_size,
            split_gb: cli.split,
            gzip: cli.gzip,
            dedup: cli.dedup,
            shuffle: cli.shuffle,
            format,
            progress: cli.progress,
            dry_run: cli.dry_run,
            verbose: cli.verbose,
        };
        cfg.validate()?;
        Ok(cfg)
    }

    pub fn validate(&self) -> Result<(), ConfigError> {
        if self.chunk_size == 0 {
            return Err(ConfigError::InvalidChunkSize);
        }
        if self.threads == 0 {
            return Err(ConfigError::InvalidThreads);
        }
        if self.buffer_mb == 0 {
            return Err(ConfigError::InvalidBufferSize);
        }
        if self.dry_run && self.split_gb.is_some() {
            return Err(ConfigError::ConflictingFlags(
                "--dry-run không dùng cùng --split".into(),
            ));
        }
        if self.dry_run && self.gzip {
            return Err(ConfigError::ConflictingFlags(
                "--dry-run không dùng cùng --gzip".into(),
            ));
        }
        Ok(())
    }

    /// Kiểm tra overflow cho domains × usernames.
    pub fn check_overflow(&self, domains: u64, usernames: u64) -> Result<u64, ConfigError> {
        domains
            .checked_mul(usernames)
            .ok_or(ConfigError::Overflow { domains, usernames })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn base_config() -> Config {
        Config {
            domains_path: PathBuf::from("d.txt"),
            usernames_path: PathBuf::from("u.txt"),
            output_path: PathBuf::from("o.txt"),
            chunk_size: 2000,
            threads: 4,
            buffer_mb: 64,
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
    fn format_parses_lowercase() {
        assert_eq!(OutputFormat::from_str("plain").unwrap(), OutputFormat::Plain);
        assert_eq!(OutputFormat::from_str("CSV").unwrap(), OutputFormat::Csv);
        assert_eq!(OutputFormat::from_str("Json").unwrap(), OutputFormat::Json);
    }

    #[test]
    fn format_rejects_unknown() {
        assert!(matches!(
            OutputFormat::from_str("xml"),
            Err(ConfigError::InvalidFormat(_))
        ));
    }

    #[test]
    fn valid_config_passes() {
        assert!(base_config().validate().is_ok());
    }

    #[test]
    fn chunk_size_zero_rejected() {
        let mut c = base_config();
        c.chunk_size = 0;
        assert!(matches!(c.validate(), Err(ConfigError::InvalidChunkSize)));
    }

    #[test]
    fn dry_run_with_split_rejected() {
        let mut c = base_config();
        c.dry_run = true;
        c.split_gb = Some(1);
        assert!(matches!(c.validate(), Err(ConfigError::ConflictingFlags(_))));
    }

    #[test]
    fn dry_run_with_gzip_rejected() {
        let mut c = base_config();
        c.dry_run = true;
        c.gzip = true;
        assert!(matches!(c.validate(), Err(ConfigError::ConflictingFlags(_))));
    }

    #[test]
    fn overflow_check() {
        let c = base_config();
        assert!(c.check_overflow(1_000_000, 200).is_ok());
        assert!(matches!(
            c.check_overflow(u64::MAX, 2),
            Err(ConfigError::Overflow { .. })
        ));
    }
}
