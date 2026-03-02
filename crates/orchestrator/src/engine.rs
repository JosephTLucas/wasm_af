use crate::host::HostState;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::time::Duration;
use tracing::info;
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
    pub max_http_bytes: Option<i64>,
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
            max_http_bytes: None,
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
        config.consume_fuel(true);

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

    /// Evict a path from the component cache.
    /// Must be called after overwriting or removing an external agent's .wasm file
    /// so that the next invocation reloads from disk rather than serving stale code.
    pub fn evict_component(&self, wasm_path: &Path) {
        self.component_cache.write().unwrap().remove(wasm_path);
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
            wasmtime_wasi_http::add_only_http_to_linker_sync(&mut linker)?;
        }
        wit::host_config::add_to_linker::<_, HasSelf<_>>(&mut linker, |x| x)?;

        let mem_bytes = opts.max_mem_pages as usize * 65536;
        host_state.store_limits = StoreLimitsBuilder::new().memory_size(mem_bytes).build();

        let mut store = Store::new(&self.engine, host_state);
        store.limiter(|state| &mut state.store_limits);

        // Fuel-based timeout: ~10M instructions per second of timeout.
        // Each WASM instruction consumes 1 fuel. This is per-Store so
        // concurrent agents cannot interfere with each other.
        let fuel_budget = opts.timeout.as_secs().max(1) * 10_000_000;
        store.set_fuel(fuel_budget).ok();

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

    /// Stricter validation for BYOA uploads: rejects components that import
    /// host interfaces beyond what agent-untrusted provides (host-config + WASI).
    pub fn validate_byoa_wasm(&self, wasm_bytes: &[u8]) -> Result<(), anyhow::Error> {
        self.validate_wasm(wasm_bytes)?;

        let component = Component::new(&self.engine, wasm_bytes)?;
        let ty = component.component_type();

        let restricted_prefixes = [
            "wasm-af:agent/host-llm",
            "wasm-af:agent/host-kv",
            "wasm-af:agent/host-exec",
            "wasm-af:agent/host-sandbox",
            "wasm-af:agent/host-email",
            "wasi:http",
        ];

        for (name, _) in ty.imports(&self.engine) {
            for prefix in &restricted_prefixes {
                if name.contains(prefix) {
                    anyhow::bail!(
                        "BYOA component imports restricted interface {name:?}; \
                         untrusted agents may only use host-config and WASI"
                    );
                }
            }
        }
        Ok(())
    }
}

/// Normalize a path by resolving `.` and `..` components (like Go's path.Clean).
fn clean_path(p: &str) -> String {
    let mut parts: Vec<&str> = Vec::new();
    for component in p.split('/') {
        match component {
            "" | "." => {}
            ".." => {
                parts.pop();
            }
            other => parts.push(other),
        }
    }
    if p.starts_with('/') {
        format!("/{}", parts.join("/"))
    } else {
        parts.join("/")
    }
}

/// Validate and normalize allowed_paths before mounting into a WASI context.
/// Rejects empty keys/values, relative paths, and guest root mounts.
pub fn sanitize_allowed_paths(
    raw: &HashMap<String, String>,
) -> Result<HashMap<String, String>, anyhow::Error> {
    let mut out = HashMap::new();
    for (host_raw, guest_raw) in raw {
        if host_raw.is_empty() {
            anyhow::bail!("allowed_paths: empty host path");
        }
        if guest_raw.is_empty() {
            anyhow::bail!("allowed_paths: empty guest path");
        }
        if !host_raw.starts_with('/') {
            anyhow::bail!("allowed_paths: host path must be absolute, got {host_raw:?}");
        }
        if !guest_raw.starts_with('/') {
            anyhow::bail!("allowed_paths: guest path must be absolute, got {guest_raw:?}");
        }

        let host_clean = clean_path(host_raw);
        let guest_clean = clean_path(guest_raw);

        if guest_clean == "/" {
            anyhow::bail!("allowed_paths: guest path must not be root (/), got {guest_raw:?}");
        }

        out.insert(host_clean, guest_clean);
    }
    Ok(out)
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use tempfile::TempDir;

    fn s(v: &str) -> String {
        v.to_string()
    }

    // ---- HostCapability::from_name ----

    #[test]
    fn capability_from_name_all_variants() {
        assert_eq!(
            HostCapability::from_name("llm_complete"),
            Some(HostCapability::Llm)
        );
        assert_eq!(
            HostCapability::from_name("kv_get"),
            Some(HostCapability::Kv)
        );
        assert_eq!(
            HostCapability::from_name("kv_put"),
            Some(HostCapability::Kv)
        );
        assert_eq!(
            HostCapability::from_name("exec_command"),
            Some(HostCapability::Exec)
        );
        assert_eq!(
            HostCapability::from_name("sandbox_exec"),
            Some(HostCapability::Sandbox)
        );
        assert_eq!(
            HostCapability::from_name("send_email"),
            Some(HostCapability::Email)
        );
        assert_eq!(
            HostCapability::from_name("http"),
            Some(HostCapability::Http)
        );
    }

    #[test]
    fn capability_from_name_unknown() {
        assert_eq!(HostCapability::from_name("unknown"), None);
        assert_eq!(HostCapability::from_name(""), None);
    }

    // ---- resolve_capabilities ----

    #[test]
    fn resolve_deduplicates_kv() {
        let caps = resolve_capabilities(&[s("kv_get"), s("kv_put")]);
        assert_eq!(caps.len(), 1);
        assert_eq!(caps[0], HostCapability::Kv);
    }

    #[test]
    fn resolve_multiple_capabilities() {
        let caps = resolve_capabilities(&[s("llm_complete"), s("exec_command"), s("http")]);
        assert_eq!(caps.len(), 3);
        assert!(caps.contains(&HostCapability::Llm));
        assert!(caps.contains(&HostCapability::Exec));
        assert!(caps.contains(&HostCapability::Http));
    }

    #[test]
    fn resolve_empty_input() {
        let caps = resolve_capabilities(&[]);
        assert!(caps.is_empty());
    }

    #[test]
    fn resolve_ignores_unknown() {
        let caps = resolve_capabilities(&[s("llm_complete"), s("bogus"), s("http")]);
        assert_eq!(caps.len(), 2);
    }

    // ---- PluginOpts defaults ----

    #[test]
    fn plugin_opts_defaults() {
        let opts = PluginOpts::default();
        assert_eq!(opts.max_mem_pages, 256);
        assert_eq!(opts.max_http_bytes, None);
        assert_eq!(opts.timeout, Duration::from_secs(30));
        assert!(opts.allowed_hosts.is_empty());
        assert!(opts.host_fn_names.is_empty());
    }

    // ---- wasm_path validation ----

    #[test]
    fn wasm_path_rejects_special_chars() {
        let dir = TempDir::new().unwrap();
        let engine = WasmEngine::new(dir.path()).unwrap();
        assert!(engine.wasm_path("../escape").is_err());
        assert!(engine.wasm_path("foo/bar").is_err());
        assert!(engine.wasm_path("agent;rm").is_err());
        assert!(engine.wasm_path("agent name").is_err());
    }

    #[test]
    fn wasm_path_allows_valid_names() {
        let dir = TempDir::new().unwrap();
        fs::write(dir.path().join("my_agent.wasm"), b"fake").unwrap();
        let engine = WasmEngine::new(dir.path()).unwrap();
        let path = engine.wasm_path("my_agent").unwrap();
        assert!(path.ends_with("my_agent.wasm"));
    }

    #[test]
    fn wasm_path_allows_hyphens() {
        let dir = TempDir::new().unwrap();
        fs::write(dir.path().join("web-search.wasm"), b"fake").unwrap();
        let engine = WasmEngine::new(dir.path()).unwrap();
        assert!(engine.wasm_path("web-search").is_ok());
    }

    #[test]
    fn wasm_path_not_found() {
        let dir = TempDir::new().unwrap();
        let engine = WasmEngine::new(dir.path()).unwrap();
        let err = engine.wasm_path("nonexistent").unwrap_err();
        assert!(err.to_string().contains("not found"));
    }

    #[test]
    fn wasm_path_checks_external_dir() {
        let dir = TempDir::new().unwrap();
        let ext = dir.path().join("external");
        fs::create_dir(&ext).unwrap();
        fs::write(ext.join("ext_agent.wasm"), b"fake").unwrap();
        let engine = WasmEngine::new(dir.path()).unwrap();
        assert!(engine.wasm_path("ext_agent").is_ok());
    }

    #[test]
    fn wasm_path_prefers_root_over_external() {
        let dir = TempDir::new().unwrap();
        fs::write(dir.path().join("agent.wasm"), b"root").unwrap();
        let ext = dir.path().join("external");
        fs::create_dir(&ext).unwrap();
        fs::write(ext.join("agent.wasm"), b"ext").unwrap();
        let engine = WasmEngine::new(dir.path()).unwrap();
        let path = engine.wasm_path("agent").unwrap();
        // Should resolve to root, not external
        assert!(!path.to_string_lossy().contains("external"));
    }

    // ---- Component cache ----

    #[test]
    fn component_cache_starts_empty() {
        let dir = TempDir::new().unwrap();
        let engine = WasmEngine::new(dir.path()).unwrap();
        assert!(engine.component_cache.read().unwrap().is_empty());
    }

    /// Minimal valid empty WASM component (magic + component-model layer version).
    const EMPTY_COMPONENT: &[u8] = &[0x00, 0x61, 0x73, 0x6d, 0x0d, 0x00, 0x01, 0x00];

    #[test]
    fn evict_component_removes_cached_entry() {
        let dir = TempDir::new().unwrap();
        let wasm_path = dir.path().join("stub.wasm");
        fs::write(&wasm_path, EMPTY_COMPONENT).unwrap();

        let engine = WasmEngine::new(dir.path()).unwrap();
        // Populate the cache by loading the component.
        engine.get_component(&wasm_path).unwrap();
        assert!(engine
            .component_cache
            .read()
            .unwrap()
            .contains_key(&wasm_path));

        engine.evict_component(&wasm_path);
        assert!(!engine
            .component_cache
            .read()
            .unwrap()
            .contains_key(&wasm_path));
    }

    #[test]
    fn evict_component_is_noop_for_unknown_path() {
        let dir = TempDir::new().unwrap();
        let engine = WasmEngine::new(dir.path()).unwrap();
        // Should not panic when evicting a path that was never cached.
        engine.evict_component(&dir.path().join("nonexistent.wasm"));
        assert!(engine.component_cache.read().unwrap().is_empty());
    }

    // ---- sanitize_allowed_paths ----

    #[test]
    fn sanitize_valid_paths() {
        let mut input = HashMap::new();
        input.insert(
            "/tmp/workspace/../workspace".to_string(),
            "/sandbox/./work".to_string(),
        );
        let result = sanitize_allowed_paths(&input).unwrap();
        assert_eq!(result.get("/tmp/workspace").unwrap(), "/sandbox/work");
    }

    #[test]
    fn sanitize_rejects_empty_host_path() {
        let mut input = HashMap::new();
        input.insert("".to_string(), "/sandbox".to_string());
        assert!(sanitize_allowed_paths(&input).is_err());
    }

    #[test]
    fn sanitize_rejects_empty_guest_path() {
        let mut input = HashMap::new();
        input.insert("/tmp/workspace".to_string(), "".to_string());
        assert!(sanitize_allowed_paths(&input).is_err());
    }

    #[test]
    fn sanitize_rejects_relative_host_path() {
        let mut input = HashMap::new();
        input.insert("../workspace".to_string(), "/sandbox".to_string());
        let err = sanitize_allowed_paths(&input).unwrap_err();
        assert!(err.to_string().contains("absolute"));
    }

    #[test]
    fn sanitize_rejects_relative_guest_path() {
        let mut input = HashMap::new();
        input.insert("/tmp/workspace".to_string(), "sandbox".to_string());
        let err = sanitize_allowed_paths(&input).unwrap_err();
        assert!(err.to_string().contains("absolute"));
    }

    #[test]
    fn sanitize_rejects_guest_root() {
        let mut input = HashMap::new();
        input.insert("/tmp/workspace".to_string(), "/".to_string());
        let err = sanitize_allowed_paths(&input).unwrap_err();
        assert!(err.to_string().contains("root"));
    }

    #[test]
    fn sanitize_empty_map_is_ok() {
        let input = HashMap::new();
        let result = sanitize_allowed_paths(&input).unwrap();
        assert!(result.is_empty());
    }
}
