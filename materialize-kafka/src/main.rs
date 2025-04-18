use anyhow::Context;
use materialize_kafka::{run_connector, Input, Output};

fn main() -> anyhow::Result<()> {
    let runtime = start_runtime()?;

    let result = runtime.block_on(run_connector(Input::new(), Output::new()));

    if let Err(err) = result.as_ref() {
        tracing::error!(error = %err, "operation failed");
    } else {
        tracing::debug!("connector run successful");
    }

    runtime.shutdown_background();

    tracing::info!(success = %result.is_ok(), "connector exiting");
    result
}

fn start_runtime() -> anyhow::Result<tokio::runtime::Runtime> {
    // The level string "debug" results in enabling debug logging for all the crates
    // in the dependency tree, which produces a ridiculous amount of output.
    // So map the debug level to a filter that will still use info level for other crates.
    let level_str = match std::env::var("LOG_LEVEL").ok() {
        Some(lvl) if lvl.as_str() == "debug" => "materialize_kafka=debug,info".to_string(),
        Some(other) => other,
        None => "info".to_string(),
    };

    let log_level = tracing_subscriber::EnvFilter::builder().parse_lossy(level_str);
    tracing_subscriber::fmt()
        .with_writer(std::io::stderr)
        .with_env_filter(log_level)
        .json()
        .flatten_event(true)
        .with_timer(tracing_subscriber::fmt::time::UtcTime::rfc_3339())
        .with_span_events(tracing_subscriber::fmt::format::FmtSpan::CLOSE)
        .with_current_span(true)
        .with_span_list(false)
        .with_thread_ids(false)
        .with_thread_names(false)
        .with_target(false)
        .init();

    // These bits about the runtime and shutdown were cargo-culted over from connector-init
    let runtime = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .context("building tokio runtime")?;
    Ok(runtime)
}
