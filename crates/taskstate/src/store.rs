use crate::types::{AuditEvent, TaskState};
use async_nats::jetstream;
use chrono::Utc;
use std::sync::atomic::{AtomicU64, Ordering};
use thiserror::Error;
use tracing::warn;

const BUCKET_TASKS: &str = "wasm-af-tasks";
const BUCKET_AUDIT: &str = "wasm-af-audit";
const BUCKET_PAYLOADS: &str = "wasm-af-payloads";

const MAX_CAS_RETRIES: u32 = 5;

#[derive(Debug, Error)]
pub enum StoreError {
    #[error("nats: {0}")]
    Nats(String),
    #[error("serialization: {0}")]
    Serde(#[from] serde_json::Error),
    #[error("not found: {0}")]
    NotFound(String),
    #[error("CAS conflict after {0} retries for task {1}")]
    CasConflict(u32, String),
    #[error("update aborted: {0}")]
    UpdateAborted(String),
}

impl From<async_nats::Error> for StoreError {
    fn from(e: async_nats::Error) -> Self {
        StoreError::Nats(e.to_string())
    }
}

pub struct Store {
    tasks: jetstream::kv::Store,
    audit: jetstream::kv::Store,
    payloads: jetstream::kv::Store,
    audit_seq: AtomicU64,
}

impl Store {
    pub async fn new(js: jetstream::Context) -> Result<Self, StoreError> {
        let tasks = js
            .create_key_value(jetstream::kv::Config {
                bucket: BUCKET_TASKS.to_string(),
                description: "wasm-af task states".to_string(),
                history: 10,
                ..Default::default()
            })
            .await
            .map_err(|e| StoreError::Nats(format!("tasks bucket: {e}")))?;

        let audit = js
            .create_key_value(jetstream::kv::Config {
                bucket: BUCKET_AUDIT.to_string(),
                description: "wasm-af immutable audit log".to_string(),
                history: 1,
                max_value_size: 64 * 1024,
                ..Default::default()
            })
            .await
            .map_err(|e| StoreError::Nats(format!("audit bucket: {e}")))?;

        let payloads = js
            .create_key_value(jetstream::kv::Config {
                bucket: BUCKET_PAYLOADS.to_string(),
                description: "wasm-af step input/output payloads".to_string(),
                history: 1,
                max_value_size: 4 * 1024 * 1024,
                ..Default::default()
            })
            .await
            .map_err(|e| StoreError::Nats(format!("payloads bucket: {e}")))?;

        Ok(Store {
            tasks,
            audit,
            payloads,
            audit_seq: AtomicU64::new(0),
        })
    }

    pub async fn put(&self, state: &mut TaskState) -> Result<(), StoreError> {
        state.updated_at = Utc::now();
        let bytes = serde_json::to_vec(state)?;
        self.tasks
            .put(&state.task_id, bytes.into())
            .await
            .map_err(|e| StoreError::Nats(format!("put {}: {e}", state.task_id)))?;
        Ok(())
    }

    pub async fn get(&self, task_id: &str) -> Result<TaskState, StoreError> {
        let entry = self
            .tasks
            .entry(task_id)
            .await
            .map_err(|e| StoreError::Nats(format!("get {task_id}: {e}")))?;
        match entry {
            Some(entry) => {
                let state: TaskState = serde_json::from_slice(&entry.value)?;
                Ok(state)
            }
            None => Err(StoreError::NotFound(task_id.to_string())),
        }
    }

    /// Read-modify-write with optimistic CAS. Retries up to MAX_CAS_RETRIES
    /// on revision conflicts.
    pub async fn update<F>(&self, task_id: &str, f: F) -> Result<(), StoreError>
    where
        F: Fn(&mut TaskState) -> Result<(), String>,
    {
        for attempt in 0..MAX_CAS_RETRIES {
            let entry = self
                .tasks
                .entry(task_id)
                .await
                .map_err(|e| StoreError::Nats(format!("cas read (attempt {}): {e}", attempt + 1)))?;

            let entry = match entry {
                Some(e) => e,
                None => return Err(StoreError::NotFound(task_id.to_string())),
            };

            let revision = entry.revision;
            let mut state: TaskState = serde_json::from_slice(&entry.value)?;

            if let Err(msg) = f(&mut state) {
                return Err(StoreError::UpdateAborted(msg));
            }

            state.updated_at = Utc::now();
            let bytes = serde_json::to_vec(&state)?;

            match self.tasks.update(task_id, bytes.into(), revision).await {
                Ok(_) => return Ok(()),
                Err(e) => {
                    let err_str = e.to_string();
                    if err_str.contains("wrong last sequence") {
                        warn!(task_id, attempt, "CAS conflict, retrying");
                        continue;
                    }
                    return Err(StoreError::Nats(format!("cas write: {err_str}")));
                }
            }
        }
        Err(StoreError::CasConflict(MAX_CAS_RETRIES, task_id.to_string()))
    }

    pub async fn delete(&self, task_id: &str) -> Result<(), StoreError> {
        self.tasks
            .delete(task_id)
            .await
            .map_err(|e| StoreError::Nats(format!("delete {task_id}: {e}")))?;
        Ok(())
    }

    pub async fn append_audit(&self, event: &mut AuditEvent) -> Result<(), StoreError> {
        if event.timestamp == chrono::DateTime::UNIX_EPOCH {
            event.timestamp = Utc::now();
        }
        let bytes = serde_json::to_vec(event)?;
        let seq = self.audit_seq.fetch_add(1, Ordering::Relaxed);
        let key = format!(
            "{}.{}.{}",
            event.task_id,
            event.timestamp.timestamp_nanos_opt().unwrap_or(0),
            seq
        );
        self.audit
            .put(&key, bytes.into())
            .await
            .map_err(|e| StoreError::Nats(format!("audit put: {e}")))?;
        Ok(())
    }

    pub async fn put_payload(&self, key: &str, payload: &str) -> Result<(), StoreError> {
        let bytes: bytes::Bytes = payload.as_bytes().to_vec().into();
        self.payloads
            .put(key, bytes)
            .await
            .map_err(|e| StoreError::Nats(format!("payload put {key}: {e}")))?;
        Ok(())
    }

    pub async fn get_payload(&self, key: &str) -> Result<String, StoreError> {
        let entry = self
            .payloads
            .entry(key)
            .await
            .map_err(|e| StoreError::Nats(format!("payload get {key}: {e}")))?;
        match entry {
            Some(e) => Ok(String::from_utf8_lossy(&e.value).to_string()),
            None => Err(StoreError::NotFound(key.to_string())),
        }
    }

    pub async fn delete_payload(&self, key: &str) -> Result<(), StoreError> {
        self.payloads
            .delete(key)
            .await
            .map_err(|e| StoreError::Nats(format!("payload delete {key}: {e}")))?;
        Ok(())
    }

    pub fn tasks_kv(&self) -> &jetstream::kv::Store {
        &self.tasks
    }
}
