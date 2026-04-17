// Library entry — expose modules cho integration tests và benchmarks.
// Binary main.rs import từ đây thay vì declare module riêng.

pub mod config;
pub mod error;
pub mod generator;
pub mod reader;
pub mod splitter;
pub mod stats;
pub mod writer;
