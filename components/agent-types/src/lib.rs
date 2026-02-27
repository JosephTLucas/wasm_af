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

#[derive(serde::Serialize)]
pub struct LlmRequest {
    pub model: String,
    pub messages: Vec<LlmMessage>,
    pub max_tokens: u32,
    pub temperature: Option<f32>,
}

#[derive(serde::Serialize)]
pub struct LlmMessage {
    pub role: String,
    pub content: String,
}

#[derive(serde::Deserialize)]
#[allow(dead_code)]
pub struct LlmResponse {
    pub content: String,
    pub model_used: String,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn deserialize_task_input_full() {
        let json = r#"{
            "task_id": "t1",
            "step_id": "s1",
            "payload": "{\"query\":\"hello\"}",
            "context": [{"key": "k", "val": "v"}]
        }"#;
        let input: TaskInput = serde_json::from_str(json).unwrap();
        assert_eq!(input.task_id, "t1");
        assert_eq!(input.step_id, "s1");
        assert!(input.payload.contains("hello"));
        assert_eq!(input.context.len(), 1);
        assert_eq!(input.context[0].key, "k");
    }

    #[test]
    fn deserialize_task_input_minimal() {
        let json = r#"{"payload": "test"}"#;
        let input: TaskInput = serde_json::from_str(json).unwrap();
        assert_eq!(input.task_id, "");
        assert_eq!(input.step_id, "");
        assert_eq!(input.payload, "test");
        assert!(input.context.is_empty());
    }

    #[test]
    fn serialize_task_output() {
        let output = TaskOutput {
            payload: "result".to_string(),
            metadata: vec![KVPair {
                key: "k".to_string(),
                val: "v".to_string(),
            }],
        };
        let json = serde_json::to_string(&output).unwrap();
        assert!(json.contains("result"));
        assert!(json.contains("\"key\":\"k\""));
    }

    #[test]
    fn serialize_task_output_empty_metadata() {
        let output = TaskOutput {
            payload: "{}".to_string(),
            metadata: vec![],
        };
        let json = serde_json::to_string(&output).unwrap();
        let roundtrip: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(roundtrip["metadata"], serde_json::json!([]));
    }

    #[test]
    fn kvpair_roundtrip() {
        let pair = KVPair {
            key: "test_key".to_string(),
            val: "test_val".to_string(),
        };
        let json = serde_json::to_string(&pair).unwrap();
        let back: KVPair = serde_json::from_str(&json).unwrap();
        assert_eq!(back.key, "test_key");
        assert_eq!(back.val, "test_val");
    }
}
