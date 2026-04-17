// Criterion benchmark — so sánh throughput ở các chunk_size khác nhau.

use std::sync::Arc;

use criterion::{black_box, criterion_group, criterion_main, BenchmarkId, Criterion, Throughput};
use crossbeam_channel::bounded;

use email_gen::config::{Config, OutputFormat};
use email_gen::generator;

fn make_test_data(n_domains: usize, n_users: usize) -> (Vec<Vec<u8>>, Arc<Vec<String>>) {
    let domains: Vec<Vec<u8>> = (0..n_domains)
        .map(|i| format!("domain{}.example.com", i).into_bytes())
        .collect();
    let users: Vec<String> = (0..n_users).map(|i| format!("user{}", i)).collect();
    (domains, Arc::new(users))
}

fn base_config(chunk_size: usize) -> Config {
    use std::path::PathBuf;
    Config {
        domains_path: PathBuf::from("x"),
        usernames_path: PathBuf::from("x"),
        output_path: PathBuf::from("x"),
        chunk_size,
        threads: num_cpus::get(),
        buffer_mb: 64,
        split_gb: None,
        gzip: false,
        dedup: false,
        shuffle: false,
        format: OutputFormat::Plain,
        progress: false,
        dry_run: true,
        verbose: false,
    }
}

fn bench_chunk_size(c: &mut Criterion) {
    let (domains, users) = make_test_data(10_000, 100);
    let domain_refs: Vec<&[u8]> = domains.iter().map(|v| v.as_slice()).collect();
    let total_emails = (domains.len() * users.len()) as u64;

    let mut group = c.benchmark_group("chunk_size");
    // Throughput = emails/sec. Dùng bytes xấp xỉ 30 bytes/email.
    group.throughput(Throughput::Bytes(total_emails * 30));

    for &cs in &[500usize, 2_000, 5_000, 20_000] {
        group.bench_with_input(BenchmarkId::from_parameter(cs), &cs, |b, &cs| {
            let cfg = base_config(cs);
            b.iter(|| {
                // Spawn receiver drain để không block sender
                let (tx, rx) = bounded::<Vec<u8>>(16);
                let drain = std::thread::spawn(move || {
                    let mut total = 0usize;
                    while let Ok(v) = rx.recv() {
                        total += v.len();
                    }
                    total
                });
                let total = generator::run(
                    black_box(&domain_refs),
                    users.clone(),
                    &cfg,
                    tx,
                    None,
                )
                .expect("generator failed");
                let _ = drain.join();
                total
            });
        });
    }
    group.finish();
}

criterion_group!(benches, bench_chunk_size);
criterion_main!(benches);
