// Entry point. Wires config → reader → generator → writer → stats.

use std::time::Instant;

use anyhow::{Context, Result};
use clap::Parser;
use crossbeam_channel::bounded;
use indicatif::{MultiProgress, ProgressBar, ProgressStyle};

use email_gen::config::{Cli, Config};
use email_gen::stats::{output_total_size, ram_peak_mb, Stats};
use email_gen::{generator, reader, writer};

fn main() -> Result<()> {
    let cli = Cli::parse();
    let cfg = Config::from_cli(cli).context("invalid CLI config")?;

    if cfg.verbose {
        eprintln!("[email-gen] config = {cfg:#?}");
    }

    // Set rayon global thread pool.
    rayon::ThreadPoolBuilder::new()
        .num_threads(cfg.threads)
        .build_global()
        .context("failed to init rayon thread pool")?;

    let start = Instant::now();

    // 1. Load usernames (validate UTF-8).
    let usernames = reader::load_usernames(&cfg.usernames_path)
        .with_context(|| format!("reading usernames from {}", cfg.usernames_path.display()))?;

    // 2. Mmap domains.
    let domains_mapped = reader::map(&cfg.domains_path)
        .with_context(|| format!("reading domains from {}", cfg.domains_path.display()))?;
    let domain_offsets = reader::line_offsets(domains_mapped.as_bytes());
    let domain_slices = reader::domain_slices(domains_mapped.as_bytes(), &domain_offsets, cfg.dedup);

    if cfg.verbose {
        eprintln!(
            "[email-gen] domains loaded: {} (after dedup: {})",
            domain_offsets.len(),
            domain_slices.len()
        );
        eprintln!("[email-gen] usernames loaded: {}", usernames.len());
    }

    // 3. Check overflow.
    let total_emails = cfg
        .check_overflow(domain_slices.len() as u64, usernames.len() as u64)
        .context("emails count overflow")?;

    // 4. Progress bars.
    let mp = MultiProgress::new();
    let (pb_chunks, pb_bytes) = if cfg.progress {
        let pbc = mp.add(ProgressBar::new(domain_slices.len() as u64));
        pbc.set_style(
            ProgressStyle::with_template(
                "{spinner} {bar:40.cyan/blue} {pos:>10}/{len} domains {elapsed_precise}",
            )
            .unwrap(),
        );
        let pbb = mp.add(ProgressBar::new(
            // Ước lượng ~40 bytes/email để có ETA.
            total_emails * 40,
        ));
        pbb.set_style(
            ProgressStyle::with_template(
                "{spinner} {bar:40.green/white} {bytes:>10}/{total_bytes} {binary_bytes_per_sec}",
            )
            .unwrap(),
        );
        (Some(pbc), Some(pbb))
    } else {
        (None, None)
    };

    // 5. Spawn writer thread.
    let (tx, rx) = bounded::<Vec<u8>>(16);
    let (writer_handle, _counter) = writer::spawn_writer(rx, &cfg, pb_bytes)
        .context("failed to spawn writer thread")?;

    // 6. Generate.
    let generated = generator::run(&domain_slices, usernames.clone(), &cfg, tx, pb_chunks)
        .context("generator failed")?;

    // 7. Join writer.
    let writer_result = writer_handle
        .join()
        .map_err(|_| anyhow::anyhow!("writer thread panicked"))?;
    let writer_stats = writer_result.context("writer returned error")?;

    let elapsed = start.elapsed();

    // 8. Tính file size thật.
    let effective_path = writer::effective_output_path(&cfg);
    let file_size = if cfg.dry_run {
        writer_stats.bytes_written
    } else {
        output_total_size(&effective_path, cfg.split_gb.is_some())
    };

    let s = Stats {
        domains: domain_slices.len() as u64,
        usernames: usernames.len() as u64,
        emails: generated,
        output: effective_path,
        file_size,
        elapsed,
        threads: cfg.threads,
        ram_peak_mb: ram_peak_mb(),
    };
    s.print_vi();

    Ok(())
}
