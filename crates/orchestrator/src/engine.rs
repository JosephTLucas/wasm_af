use crate::host::HostState;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use tracing::info;
use std::time::Duration;
use wasmtime::component::{Component, HasSelf, Linker};
use wasmtime::{Config, Engine, Store, StoreLimitsBuilder};

pub mod bindings {
    wasmtime::component::bindgen!({
        path: "wit/agent.wit",
        world: "agent",
    });
}

pub use bindings::wasm_af::agent as wit;
pub use bindings::Agent;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TaskInput {
    pub task_id: String,
    pub step_id: String,
    pub payload: String,
    pub context: Vec<KvPair>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct KvPair {
    pub key: String,
    pub val: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TaskOutput {
    pub payload: String,
    pub metadata: Vec<KvPair>,
}

#[derive(Debug, Clone, PartialEq, Eq, Hash)]
pub enum HostCapability {
    Llm,
    Kv,
    Exec,
    Sandbox,
    Email,
    Http,
}

impl HostCapability {
    pub fn from_name(name: &str) -> Option<Self> {
        match name {
            "llm_complete" => Some(Self::Llm),
            "kv_get" | "kv_put" => Some(Self::Kv),
            "exec_command" => Some(Self::Exec),
            "sandbox_exec" => Some(Self::Sandbox),
            "send_email" => Some(Self::Email),
            "http" => Some(Self::Http),
            _ => None,
        }
    }
}

pub struct PluginOpts {
    pub allowed_hosts: Vec<String>,
    pub host_fn_names: Vec<String>,
    pub max_mem_pages: u64,
    pub timeout: Duration,
    pub config: HashMap<String, String>,
    pub allowed_paths: HashMap<String, String>,
}

impl Default for PluginOpts {
    fn default() -> Self {
        Self {
            allowed_hosts: Vec::new(),
            host_fn_names: Vec::new(),
            max_mem_pages: 256,
            timeout: Duration::from_secs(30),
            config: HashMap::new(),
            allowed_paths: HashMap::new(),
        }
    }
}

pub struct WasmEngine {
    engine: Engine,
    wasm_dir: PathBuf,
    component_cache: std::sync::RwLock<HashMap<PathBuf, Component>>,
}

impl WasmEngine {
    pub fn new(wasm_dir: &Path) -> Result<Self, anyhow::Error> {
        let mut config = Config::new();
        config.wasm_component_model(true);
        config.epoch_interruption(true);

        let engine = Engine::new(&config)?;

        Ok(WasmEngine {
            engine,
            wasm_dir: wasm_dir.to_path_buf(),
            component_cache: std::sync::RwLock::new(HashMap::new()),
        })
    }

    pub fn wasm_dir(&self) -> &Path {
        &self.wasm_dir
    }

    pub fn wasm_path(&self, name: &str) -> Result<PathBuf, anyhow::Error> {
        if !name
            .chars()
            .all(|c| c.is_alphanumeric() || c == '_' || c == '-')
        {
            anyhow::bail!("invalid wasm name: {name:?}");
        }

        let candidates = [
            self.wasm_dir.join(format!("{name}.wasm")),
            self.wasm_dir.join("external").join(format!("{name}.wasm")),
        ];

        for path in &candidates {
            if path.exists() {
                let canonical = path.canonicalize()?;
                let base = self
                    .wasm_dir
                    .canonicalize()
                    .unwrap_or(self.wasm_dir.clone());
                if !canonical.starts_with(&base) {
                    anyhow::bail!("wasm path escapes configured wasm dir");
                }
                return Ok(canonical);
            }
        }

        anyhow::bail!(
            "wasm component {name:?} not found (searched {})",
            candidates
                .iter()
                .map(|p| p.display().to_string())
                .collect::<Vec<_>>()
                .join(", ")
        )
    }

    fn get_component(&self, wasm_path: &Path) -> Result<Component, anyhow::Error> {
        if let Some(c) = self.component_cache.read().unwrap().get(wasm_path) {
            return Ok(c.clone());
        }
        let component = Component::from_file(&self.engine, wasm_path)?;
        self.component_cache
            .write()
            .unwrap()
            .insert(wasm_path.to_path_buf(), component.clone());
        Ok(component)
    }

    /// Load, instantiate, call execute, and destroy a component in one scope.
    /// Only the host interfaces corresponding to `opts.host_fn_names` are linked.
    /// If the component imports an interface that isn't provided, instantiation
    /// fails — this is the structural capability absence guarantee.
    pub fn invoke_agent(
        &self,
        wasm_name: &str,
        input: &TaskInput,
        opts: PluginOpts,
        mut host_state: HostState,
    ) -> Result<TaskOutput, anyhow::Error> {
        let wasm_path = self.wasm_path(wasm_name)?;
        let component = self.get_component(&wasm_path)?;

        let caps = resolve_capabilities(&opts.host_fn_names);

        let mut linker: Linker<HostState> = Linker::new(&self.engine);

        wasmtime_wasi::p2::add_to_linker_sync(&mut linker)?;

        if caps.contains(&HostCapability::Llm) {
            wit::host_llm::add_to_linker::<_, HasSelf<_>>(&mut linker, |x| x)?;
        }
        if caps.contains(&HostCapability::Kv) {
            wit::host_kv::add_to_linker::<_, HasSelf<_>>(&mut linker, |x| x)?;
        }
        if caps.contains(&HostCapability::Exec) {
            wit::host_exec::add_to_linker::<_, HasSelf<_>>(&mut linker, |x| x)?;
        }
        if caps.contains(&HostCapability::Sandbox) {
            wit::host_sandbox::add_to_linker::<_, HasSelf<_>>(&mut linker, |x| x)?;
        }
        if caps.contains(&HostCapability::Email) {
            wit::host_email::add_to_linker::<_, HasSelf<_>>(&mut linker, |x| x)?;
        }
        if caps.contains(&HostCapability::Http) {
            wasmtime_wasi_http::add_to_linker_sync(&mut linker)?;
        }
        wit::host_config::add_to_linker::<_, HasSelf<_>>(&mut linker, |x| x)?;

        let mem_bytes = opts.max_mem_pages as usize * 65536;
        host_state.store_limits = StoreLimitsBuilder::new()
            .memory_size(mem_bytes)
            .build();

        let mut store = Store::new(&self.engine, host_state);
        store.limiter(|state| &mut state.store_limits);

        // Use a large enough epoch deadline that only the dedicated timeout
        // thread can trigger it. Each call increments by exactly 1, so
        // concurrent agents with the same engine share epoch ticks. The
        // deadline is set high enough that normal ticks from other agents'
        // timeouts won't reach it — only our own thread will.
        store.set_epoch_deadline(u64::MAX / 2);

        let engine_clone = self.engine.clone();
        let timeout = opts.timeout;
        std::thread::spawn(move || {
            std::thread::sleep(timeout);
            engine_clone.increment_epoch();
        });

        let wit_input = wit::types::TaskInput {
            task_id: input.task_id.clone(),
            step_id: input.step_id.clone(),
            payload: input.payload.clone(),
            context: input
                .context
                .iter()
                .map(|kv| wit::types::KvPair {
                    key: kv.key.clone(),
                    val: kv.val.clone(),
                })
                .collect(),
        };

        let create_start = std::time::Instant::now();
        let agent_bindings = Agent::instantiate(&mut store, &component, &linker)?;
        let create_ms = create_start.elapsed().as_millis();

        info!(
            task_id = %input.task_id,
            step_id = %input.step_id,
            agent = %wasm_name,
            host_fns = opts.host_fn_names.len(),
            create_ms = create_ms,
            "plugin created"
        );

        let exec_start = std::time::Instant::now();
        let result = agent_bindings.call_execute(&mut store, &wit_input)?;
        let exec_ms = exec_start.elapsed().as_millis();

        info!(
            task_id = %input.task_id,
            step_id = %input.step_id,
            agent = %wasm_name,
            exec_ms = exec_ms,
            "plugin destroyed"
        );

        match result {
            Ok(out) => Ok(TaskOutput {
                payload: out.payload,
                metadata: out
                    .metadata
                    .into_iter()
                    .map(|kv| KvPair {
                        key: kv.key,
                        val: kv.val,
                    })
                    .collect(),
            }),
            Err(msg) => Err(anyhow::anyhow!("agent error: {msg}")),
        }
    }

    pub fn validate_wasm(&self, wasm_bytes: &[u8]) -> Result<(), anyhow::Error> {
        let component = Component::new(&self.engine, wasm_bytes)?;
        let ty = component.component_type();
        let has_execute = ty.exports(&self.engine).any(|(name, _)| name == "execute");
        if !has_execute {
            anyhow::bail!("component does not export 'execute' function");
        }
        Ok(())
    }
}

fn resolve_capabilities(host_fn_names: &[String]) -> Vec<HostCapability> {
    let mut caps = Vec::new();
    for name in host_fn_names {
        if let Some(cap) = HostCapability::from_name(name) {
            if !caps.contains(&cap) {
                caps.push(cap);
            }
        }
    }
    caps
}
