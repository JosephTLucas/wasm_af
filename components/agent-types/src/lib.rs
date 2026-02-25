#[derive(serde::Deserialize)]
pub struct TaskInput {
    #[serde(default)]
    pub task_id: String,
    #[serde(default)]
    pub step_id: String,
    pub payload: String,
    #[serde(default)]
    pub context: Vec<KVPair>,
}

#[derive(serde::Deserialize, serde::Serialize)]
pub struct KVPair {
    pub key: String,
    pub val: String,
}

#[derive(serde::Serialize)]
pub struct TaskOutput {
    pub payload: String,
    #[serde(default)]
    pub metadata: Vec<KVPair>,
}
