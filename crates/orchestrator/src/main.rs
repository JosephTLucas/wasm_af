mod api;
mod engine;
mod host;
mod policy;
mod registry;
mod scheduler;

use axum::routing::{delete, get, post};
use axum::Router;
use policy::{load_data_file, load_rego_modules, OpaEvaluator};
use scheduler::Orchestrator;
use std::collections::{HashMap, HashSet};
use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tracing::info;

fn env_or(key: &str, fallback: &str) -> String {
    std::env::var(key).unwrap_or_else(|_| fallback.to_string())
}

fn env_or_u64(key: &str, fallback: u64) -> u64 {
    std::env::var(key)
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(fallback)
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .json()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env().unwrap_or_else(|_| "info".into()),
        )
        .init();

    let listen_addr = env_or("LISTEN_ADDR", ":8080");
    let wasm_dir = env_or("WASM_DIR", "./components/target/wasm32-wasip2/release");
    let nats_url = env_or("NATS_URL", "nats://127.0.0.1:4222");
    let opa_policy_path = std::env::var("OPA_POLICY").unwrap_or_default();
    let opa_data_path = std::env::var("OPA_DATA").unwrap_or_default();

    let plugin_timeout_sec = env_or_u64("PLUGIN_TIMEOUT_SEC", 30);
    let plugin_max_mem_pages = env_or_u64("PLUGIN_MAX_MEMORY_PAGES", 256);

    // Agent registry
    let agent_registry_json = std::env::var("AGENT_REGISTRY")
        .or_else(|_| {
            std::env::var("AGENT_REGISTRY_FILE").and_then(|path| {
                std::fs::read_to_string(&path).map_err(|_| std::env::VarError::NotPresent)
            })
        })
        .map_err(|_| anyhow::anyhow!("AGENT_REGISTRY or AGENT_REGISTRY_FILE is required"))?;

    let registry = Arc::new(registry::AgentRegistry::parse(
        agent_registry_json.as_bytes(),
    )?);
    info!("loaded agent registry");

    // OPA policy
    if opa_policy_path.is_empty() {
        anyhow::bail!("OPA_POLICY is required: policy evaluation gates all agent execution");
    }

    let modules = load_rego_modules(&PathBuf::from(&opa_policy_path))?;
    let initial_data = if !opa_data_path.is_empty() {
        Some(load_data_file(&PathBuf::from(&opa_data_path))?)
    } else {
        None
    };
    let policy = Arc::new(Mutex::new(OpaEvaluator::new(&modules, initial_data)?));
    info!(path = %opa_policy_path, modules = modules.len(), "OPA policy loaded");

    // WASM engine
    let wasm_engine = Arc::new(engine::WasmEngine::new(&PathBuf::from(&wasm_dir))?);
    info!(wasm_dir = %wasm_dir, "WASM engine initialized");

    // NATS
    let nats_client = async_nats::connect(&nats_url).await?;
    info!(url = %nats_url, "NATS connected");

    let js = async_nats::jetstream::new(nats_client.clone());
    let store = Arc::new(wasm_af_taskstate::Store::new(js.clone()).await?);

    let config_kv = js
        .create_key_value(async_nats::jetstream::kv::Config {
            bucket: "wasm-af-config".to_string(),
            description: "wasm-af orchestrator runtime configuration".to_string(),
            ..Default::default()
        })
        .await?;

    // LLM config
    let llm_timeout_sec = env_or_u64("LLM_TIMEOUT_SEC", 120);
    let llm_client = std::sync::Arc::new(
        reqwest::blocking::Client::builder()
            .timeout(Duration::from_secs(llm_timeout_sec))
            .build()
            .expect("failed to build LLM HTTP client"),
    );
    let llm_state = host::LlmState {
        mode: env_or("LLM_MODE", "mock"),
        base_url: env_or("LLM_BASE_URL", ""),
        api_key: env_or("LLM_API_KEY", ""),
        model: env_or("LLM_MODEL", "gpt-4o-mini"),
        temperature: std::env::var("LLM_TEMPERATURE")
            .ok()
            .and_then(|v| v.parse().ok()),
        client: llm_client,
    };
    if llm_state.mode == "mock" {
        tracing::warn!(
            "LLM_MODE=mock: all LLM calls return canned responses. \
             Set LLM_MODE=api with LLM_BASE_URL and LLM_API_KEY for real inference."
        );
    }
    info!(mode = %llm_state.mode, model = %llm_state.model, "LLM configured");

    // Shell config
    let shell_allowed_cmds: HashMap<String, bool> = env_or(
        "SHELL_ALLOWED_COMMANDS",
        "ls,cat,pwd,echo,find,date,uname,wc,head,tail",
    )
    .split(',')
    .filter(|s| !s.trim().is_empty())
    .map(|s| (s.trim().to_string(), true))
    .collect();
    let shell_allowed_paths: Vec<String> = env_or("SHELL_ALLOWED_PATHS", "/tmp/wasmclaw")
        .split(',')
        .filter(|s| !s.trim().is_empty())
        .map(|s| s.trim().to_string())
        .collect();
    let shell_work_dir = shell_allowed_paths
        .first()
        .cloned()
        .unwrap_or_else(|| "/tmp".to_string());

    let exec_state = host::ExecState {
        allowed_commands: shell_allowed_cmds,
        allowed_paths: shell_allowed_paths,
        work_dir: shell_work_dir,
        timeout_secs: env_or_u64("SHELL_TIMEOUT_SEC", 10),
    };

    // Sandbox config
    let mut sandbox_config = wasmtime::Config::new();
    sandbox_config.consume_fuel(true);
    let sandbox_engine =
        std::sync::Arc::new(wasmtime::Engine::new(&sandbox_config).expect("sandbox engine"));
    let sandbox_state = host::SandboxState {
        runtimes_dir: env_or("SANDBOX_RUNTIMES_DIR", "./runtimes"),
        allowed_languages: env_or("SANDBOX_ALLOWED_LANGUAGES", "python")
            .split(',')
            .filter(|s| !s.trim().is_empty())
            .map(|s| (s.trim().to_string(), true))
            .collect(),
        allowed_paths: env_or("SANDBOX_ALLOWED_PATHS", "/tmp/wasmclaw")
            .split(',')
            .filter(|s| !s.trim().is_empty())
            .map(|s| (s.trim().to_string(), s.trim().to_string()))
            .collect(),
        timeout_secs: env_or_u64("SANDBOX_TIMEOUT_SEC", 30),
        engine: sandbox_engine,
        module_cache: std::sync::Arc::new(std::sync::RwLock::new(HashMap::new())),
    };

    // Memory KV (cached handle for agent kv_get/kv_put)
    let memory_kv = js
        .create_key_value(async_nats::jetstream::kv::Config {
            bucket: "wasm-af-memory".to_string(),
            description: "wasm-af agent memory store".to_string(),
            ..Default::default()
        })
        .await?;

    // Email config
    let email_state = host::EmailState {
        allowed_domains: env_or("EMAIL_ALLOWED_DOMAINS", "example.com,partner-corp.com")
            .split(',')
            .filter(|s| !s.trim().is_empty())
            .map(|s| (s.trim().to_lowercase(), true))
            .collect(),
    };

    let orch = Arc::new(Orchestrator {
        engine: wasm_engine,
        store: store.clone(),
        policy,
        registry: registry.clone(),
        llm_state,
        kv_state: host::KvState {
            memory_kv: Some(std::sync::Arc::new(memory_kv)),
        },
        exec_state,
        sandbox_state,
        email_state,
        plugin_timeout: Duration::from_secs(plugin_timeout_sec),
        plugin_max_mem_pages: plugin_max_mem_pages,
        config_kv,
        approval_webhook_url: env_or("APPROVAL_WEBHOOK_URL", ""),
        approval_timeout_sec: env_or_u64("APPROVAL_TIMEOUT_SEC", 0),
        running_tasks: Arc::new(tokio::sync::Mutex::new(HashSet::new())),
    });

    // Seed allowed-fetch-domains from NATS KV into OPA data store.
    if let Ok(entry) = orch.config_kv.entry("allowed-fetch-domains").await {
        if let Some(entry) = entry {
            let domains: Vec<String> = String::from_utf8_lossy(&entry.value)
                .split(',')
                .filter(|s| !s.trim().is_empty())
                .map(|s| s.trim().to_string())
                .collect();
            if let Ok(mut p) = orch.policy.lock() {
                let _ = p.update_data("/config/allowed_domains", serde_json::json!(domains));
                info!(
                    count = domains.len(),
                    "seeded allowed-fetch-domains from KV"
                );
            }
        }
    }

    // Watch for live updates to allowed-fetch-domains.
    {
        let policy = orch.policy.clone();
        let kv = orch.config_kv.clone();
        tokio::spawn(async move {
            let mut watcher = match kv.watch("allowed-fetch-domains").await {
                Ok(w) => w,
                Err(e) => {
                    tracing::error!(err = %e, "failed to start allowed-fetch-domains watcher");
                    return;
                }
            };
            use futures::StreamExt;
            while let Some(entry) = watcher.next().await {
                match entry {
                    Ok(entry) => {
                        let domains: Vec<String> = String::from_utf8_lossy(&entry.value)
                            .split(',')
                            .filter(|s| !s.trim().is_empty())
                            .map(|s| s.trim().to_string())
                            .collect();
                        if let Ok(mut p) = policy.lock() {
                            let _ = p
                                .update_data("/config/allowed_domains", serde_json::json!(domains));
                            info!(
                                count = domains.len(),
                                "allowed-fetch-domains updated from KV"
                            );
                        }
                    }
                    Err(e) => {
                        tracing::error!(err = %e, "allowed-fetch-domains watch error");
                    }
                }
            }
        });
    }

    // Watch for live updates to approved-external-agents.
    {
        let registry = orch.registry.clone();
        let kv = orch.config_kv.clone();
        tokio::spawn(async move {
            let mut watcher = match kv.watch("external-agents").await {
                Ok(w) => w,
                Err(e) => {
                    tracing::error!(err = %e, "failed to start external-agents watcher");
                    return;
                }
            };
            use futures::StreamExt;
            while let Some(entry) = watcher.next().await {
                match entry {
                    Ok(entry) => {
                        let raw = entry.value;
                        if let Ok(agents) =
                            serde_json::from_slice::<HashMap<String, serde_json::Value>>(&raw)
                        {
                            for (name, meta_val) in agents {
                                if let Ok(meta) =
                                    serde_json::from_value::<registry::AgentMeta>(meta_val)
                                {
                                    let _ = registry.register(&name, meta);
                                }
                            }
                            info!("external agents synced from KV");
                        }
                    }
                    Err(e) => {
                        tracing::error!(err = %e, "external-agents watch error");
                    }
                }
            }
        });
    }

    let app = Router::new()
        .route("/tasks", post(api::handle_submit_task))
        .route("/tasks/{id}", get(api::handle_get_task))
        .route("/tasks/{id}/approvals", get(api::handle_list_approvals))
        .route(
            "/tasks/{id}/steps/{stepId}/approve",
            post(api::handle_approve_step),
        )
        .route(
            "/tasks/{id}/steps/{stepId}/reject",
            post(api::handle_reject_step),
        )
        .route("/agents", post(api::handle_register_agent))
        .route("/agents", get(api::handle_list_agents))
        .route("/agents/{name}", delete(api::handle_remove_agent))
        .route("/healthz", get(api::handle_healthz))
        .with_state(orch);

    let addr: SocketAddr = listen_addr
        .trim_start_matches(':')
        .parse::<u16>()
        .map(|port| SocketAddr::from(([0, 0, 0, 0], port)))
        .unwrap_or_else(|_| {
            listen_addr
                .parse()
                .unwrap_or(SocketAddr::from(([0, 0, 0, 0], 8080)))
        });

    info!(addr = %addr, "HTTP server listening");

    let listener = tokio::net::TcpListener::bind(addr).await?;
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;

    info!("shutdown complete");
    Ok(())
}

async fn shutdown_signal() {
    let _ = tokio::signal::ctrl_c().await;
    info!("shutdown requested");
}
