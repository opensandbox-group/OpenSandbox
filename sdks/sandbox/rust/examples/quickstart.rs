// Copyright 2026 The OpenSandbox Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//! Creates a sandbox, streams a command, reads metrics, and cleans up.
//!
//! Run against a live server:
//!
//! ```bash
//! OPEN_SANDBOX_DOMAIN=localhost:8080 OPEN_SANDBOX_API_KEY=... \
//!   cargo run --example quickstart
//! ```

use futures_util::StreamExt;
use opensandbox::{ConnectionConfig, CreateOptions, ExecutionEvent, RunCommandRequest, Sandbox};

#[tokio::main]
async fn main() -> opensandbox::Result<()> {
    let config = ConnectionConfig::new().with_domain("localhost:8080");
    let sandbox = Sandbox::create(
        config,
        CreateOptions::from_image("python:3.12").with_timeout_seconds(600),
    )
    .await?;
    println!("sandbox ready: {}", sandbox.id());

    // Stream a command line by line instead of buffering the whole output.
    let request = RunCommandRequest {
        command: Some("for i in 1 2 3; do echo line-$i; done".to_string()),
        ..Default::default()
    };
    let mut stream = sandbox.execd().run_command(&request).await?;
    while let Some(event) = stream.next().await {
        match event? {
            ExecutionEvent::Stdout(line) => println!("stdout: {}", line.text),
            ExecutionEvent::Stderr(line) => eprintln!("stderr: {}", line.text),
            ExecutionEvent::Error(error) => eprintln!("failed: {}", error.value),
            ExecutionEvent::Complete(complete) => {
                println!("exit code: {:?}", complete.exit_code);
                break;
            }
            _ => {}
        }
    }

    let metrics = sandbox.execd().metrics().await?;
    println!(
        "cpu {} / {}, memory {} MiB / {} MiB",
        metrics.cpu_used_pct, metrics.cpu_count, metrics.mem_used_mib, metrics.mem_total_mib
    );

    // Collecting is simpler when only the final output matters.
    let buffered = sandbox.exec("python -c 'print(1 + 1)'").await?;
    print!("{}", buffered.text());

    sandbox.kill().await?;
    Ok(())
}
