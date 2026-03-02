use std::collections::{HashMap, HashSet};

#[derive(Debug, Clone)]
pub struct Graph {
    nodes: HashSet<String>,
    children: HashMap<String, Vec<String>>,
    parents: HashMap<String, Vec<String>>,
}

#[derive(Debug, thiserror::Error)]
pub enum DagError {
    #[error("duplicate node {0:?}")]
    DuplicateNode(String),
    #[error("depends_on references unknown node {0:?}")]
    UnknownDep(String),
    #[error("node {0:?} depends on unknown node {1:?}")]
    UnknownParent(String, String),
    #[error("cycle detected involving node {0:?}")]
    Cycle(String),
    #[error("node {0:?} already exists")]
    NodeExists(String),
    #[error("after node {0:?} does not exist")]
    AfterMissing(String),
    #[error("node {0:?} does not depend on {1:?}")]
    NotAChild(String, String),
}

impl Graph {
    /// Build a graph from node IDs and their dependency edges.
    /// `depends_on` maps each node to the IDs it depends on (its parents).
    pub fn new(
        node_ids: &[String],
        depends_on: &HashMap<String, Vec<String>>,
    ) -> Result<Self, DagError> {
        let mut nodes = HashSet::with_capacity(node_ids.len());
        let mut children: HashMap<String, Vec<String>> = HashMap::new();
        let mut parents: HashMap<String, Vec<String>> = HashMap::new();

        for id in node_ids {
            if !nodes.insert(id.clone()) {
                return Err(DagError::DuplicateNode(id.clone()));
            }
        }

        for (id, deps) in depends_on {
            if !nodes.contains(id) {
                return Err(DagError::UnknownDep(id.clone()));
            }
            for dep in deps {
                if !nodes.contains(dep) {
                    return Err(DagError::UnknownParent(id.clone(), dep.clone()));
                }
                parents.entry(id.clone()).or_default().push(dep.clone());
                children.entry(dep.clone()).or_default().push(id.clone());
            }
        }

        let g = Graph {
            nodes,
            children,
            parents,
        };

        if let Some(cycle_node) = g.detect_cycle() {
            return Err(DagError::Cycle(cycle_node));
        }

        Ok(g)
    }

    /// Returns node IDs whose parents are all in the completed set
    /// and that are not themselves completed.
    pub fn ready(&self, completed: &HashSet<String>) -> Vec<String> {
        let mut ready = Vec::new();
        for id in &self.nodes {
            if completed.contains(id) {
                continue;
            }
            let all_met = self
                .parents
                .get(id)
                .map(|ps| ps.iter().all(|p| completed.contains(p)))
                .unwrap_or(true);
            if all_met {
                ready.push(id.clone());
            }
        }
        ready
    }

    /// Returns all transitive ancestors (dependencies) of a node,
    /// not including the node itself.
    pub fn ancestors(&self, id: &str) -> Vec<String> {
        let mut visited = HashSet::new();
        self.walk_ancestors(id, &mut visited);
        visited.remove(id);
        visited.into_iter().collect()
    }

    fn walk_ancestors(&self, id: &str, visited: &mut HashSet<String>) {
        if !visited.insert(id.to_string()) {
            return;
        }
        if let Some(ps) = self.parents.get(id) {
            for p in ps {
                self.walk_ancestors(p, visited);
            }
        }
    }

    /// Returns the direct dependents of a node.
    pub fn children(&self, id: &str) -> Vec<String> {
        self.children.get(id).cloned().unwrap_or_default()
    }

    /// Insert a new node between `after` and the dependents listed in `rewire`.
    pub fn splice(&mut self, new_id: &str, after: &str, rewire: &[String]) -> Result<(), DagError> {
        if self.nodes.contains(new_id) {
            return Err(DagError::NodeExists(new_id.to_string()));
        }
        if !self.nodes.contains(after) {
            return Err(DagError::AfterMissing(after.to_string()));
        }

        let child_set: HashSet<&String> = self
            .children
            .get(after)
            .map(|c| c.iter().collect())
            .unwrap_or_default();
        for r in rewire {
            if !child_set.contains(r) {
                return Err(DagError::NotAChild(r.clone(), after.to_string()));
            }
        }

        self.nodes.insert(new_id.to_string());
        self.parents
            .insert(new_id.to_string(), vec![after.to_string()]);
        append_unique(
            self.children.entry(after.to_string()).or_default(),
            new_id.to_string(),
        );

        for r in rewire {
            if let Some(ps) = self.parents.get_mut(r) {
                replace_in_vec(ps, after, new_id);
            }
            if let Some(cs) = self.children.get_mut(after) {
                remove_from_vec(cs, r);
            }
            append_unique(
                self.children.entry(new_id.to_string()).or_default(),
                r.clone(),
            );
        }

        if let Some(cycle_node) = self.detect_cycle() {
            self.remove_splice(new_id, after, rewire);
            return Err(DagError::Cycle(cycle_node));
        }

        Ok(())
    }

    fn remove_splice(&mut self, new_id: &str, after: &str, rewire: &[String]) {
        for r in rewire {
            if let Some(ps) = self.parents.get_mut(r) {
                replace_in_vec(ps, new_id, after);
            }
            append_unique(
                self.children.entry(after.to_string()).or_default(),
                r.clone(),
            );
        }
        self.nodes.remove(new_id);
        self.parents.remove(new_id);
        self.children.remove(new_id);
        if let Some(cs) = self.children.get_mut(after) {
            remove_from_vec(cs, &new_id.to_string());
        }
    }

    fn detect_cycle(&self) -> Option<String> {
        const WHITE: u8 = 0;
        const GRAY: u8 = 1;
        const BLACK: u8 = 2;

        let mut color: HashMap<&str, u8> = HashMap::with_capacity(self.nodes.len());
        for id in &self.nodes {
            color.insert(id.as_str(), WHITE);
        }

        for id in &self.nodes {
            if color[id.as_str()] != WHITE {
                continue;
            }

            let mut stack: Vec<(&str, usize)> = vec![(id.as_str(), 0)];
            *color.get_mut(id.as_str()).unwrap() = GRAY;

            while let Some(top) = stack.last_mut() {
                let kids = self
                    .children
                    .get(top.0)
                    .map(|v| v.as_slice())
                    .unwrap_or(&[]);
                if top.1 >= kids.len() {
                    let node = top.0;
                    *color.get_mut(node).unwrap() = BLACK;
                    stack.pop();
                    continue;
                }
                let child = kids[top.1].as_str();
                top.1 += 1;

                match color[child] {
                    GRAY => return Some(child.to_string()),
                    WHITE => {
                        *color.get_mut(child).unwrap() = GRAY;
                        stack.push((child, 0));
                    }
                    _ => {}
                }
            }
        }
        None
    }
}

fn replace_in_vec(v: &mut [String], old: &str, new: &str) {
    for item in v.iter_mut() {
        if item == old {
            *item = new.to_string();
            return;
        }
    }
}

fn remove_from_vec(v: &mut Vec<String>, val: &str) {
    if let Some(pos) = v.iter().position(|x| x == val) {
        v.remove(pos);
    }
}

fn append_unique(v: &mut Vec<String>, val: String) {
    if !v.contains(&val) {
        v.push(val);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn ids(names: &[&str]) -> Vec<String> {
        names.iter().map(|s| s.to_string()).collect()
    }

    #[test]
    fn linear_chain() {
        let deps = HashMap::from([
            ("b".to_string(), vec!["a".to_string()]),
            ("c".to_string(), vec!["b".to_string()]),
        ]);
        let g = Graph::new(&ids(&["a", "b", "c"]), &deps).unwrap();

        let completed = HashSet::new();
        let ready = g.ready(&completed);
        assert_eq!(ready, vec!["a".to_string()]);

        let completed: HashSet<String> = ["a".to_string()].into();
        let ready = g.ready(&completed);
        assert_eq!(ready, vec!["b".to_string()]);
    }

    #[test]
    fn parallel_fan_out() {
        let deps = HashMap::from([
            ("b".to_string(), vec!["a".to_string()]),
            ("c".to_string(), vec!["a".to_string()]),
        ]);
        let g = Graph::new(&ids(&["a", "b", "c"]), &deps).unwrap();

        let completed: HashSet<String> = ["a".to_string()].into();
        let mut ready = g.ready(&completed);
        ready.sort();
        assert_eq!(ready, ids(&["b", "c"]));
    }

    #[test]
    fn cycle_detected() {
        let deps = HashMap::from([
            ("a".to_string(), vec!["b".to_string()]),
            ("b".to_string(), vec!["a".to_string()]),
        ]);
        assert!(Graph::new(&ids(&["a", "b"]), &deps).is_err());
    }

    #[test]
    fn ancestors() {
        let deps = HashMap::from([
            ("b".to_string(), vec!["a".to_string()]),
            ("c".to_string(), vec!["b".to_string()]),
        ]);
        let g = Graph::new(&ids(&["a", "b", "c"]), &deps).unwrap();
        let mut anc = g.ancestors("c");
        anc.sort();
        assert_eq!(anc, ids(&["a", "b"]));
    }

    #[test]
    fn splice_basic() {
        let deps = HashMap::from([("b".to_string(), vec!["a".to_string()])]);
        let mut g = Graph::new(&ids(&["a", "b"]), &deps).unwrap();
        g.splice("x", "a", &["b".to_string()]).unwrap();

        let completed: HashSet<String> = ["a".to_string()].into();
        let ready = g.ready(&completed);
        assert_eq!(ready, vec!["x".to_string()]);

        let completed: HashSet<String> = ["a".to_string(), "x".to_string()].into();
        let ready = g.ready(&completed);
        assert_eq!(ready, vec!["b".to_string()]);
    }

    #[test]
    fn duplicate_node() {
        let err = Graph::new(&ids(&["a", "a"]), &HashMap::new());
        assert!(err.is_err());
    }

    #[test]
    fn ready_all_independent() {
        let g = Graph::new(&ids(&["a", "b", "c"]), &HashMap::new()).unwrap();
        let mut ready = g.ready(&HashSet::new());
        ready.sort();
        assert_eq!(ready, ids(&["a", "b", "c"]));
    }

    #[test]
    fn ready_diamond_join() {
        // a -> b, a -> c, b -> d, c -> d  (d waits for both b and c)
        let deps = HashMap::from([
            ("b".to_string(), vec!["a".to_string()]),
            ("c".to_string(), vec!["a".to_string()]),
            ("d".to_string(), vec!["b".to_string(), "c".to_string()]),
        ]);
        let g = Graph::new(&ids(&["a", "b", "c", "d"]), &deps).unwrap();

        let done: HashSet<String> = ["a".into(), "b".into()].into();
        let ready = g.ready(&done);
        assert_eq!(ready, vec!["c".to_string()], "d should not be ready until both b and c complete");

        let done: HashSet<String> = ["a".into(), "b".into(), "c".into()].into();
        let ready = g.ready(&done);
        assert_eq!(ready, vec!["d".to_string()]);
    }

    #[test]
    fn ready_returns_empty_when_all_completed() {
        let deps = HashMap::from([("b".to_string(), vec!["a".to_string()])]);
        let g = Graph::new(&ids(&["a", "b"]), &deps).unwrap();
        let done: HashSet<String> = ["a".into(), "b".into()].into();
        assert!(g.ready(&done).is_empty());
    }

    #[test]
    fn children_returns_direct_dependents() {
        let deps = HashMap::from([
            ("b".to_string(), vec!["a".to_string()]),
            ("c".to_string(), vec!["a".to_string()]),
        ]);
        let g = Graph::new(&ids(&["a", "b", "c"]), &deps).unwrap();
        let mut kids = g.children("a");
        kids.sort();
        assert_eq!(kids, ids(&["b", "c"]));
    }

    #[test]
    fn children_leaf_returns_empty() {
        let deps = HashMap::from([("b".to_string(), vec!["a".to_string()])]);
        let g = Graph::new(&ids(&["a", "b"]), &deps).unwrap();
        assert!(g.children("b").is_empty());
    }

    #[test]
    fn children_unknown_returns_empty() {
        let g = Graph::new(&ids(&["a"]), &HashMap::new()).unwrap();
        assert!(g.children("nonexistent").is_empty());
    }

    #[test]
    fn ancestors_diamond() {
        let deps = HashMap::from([
            ("b".to_string(), vec!["a".to_string()]),
            ("c".to_string(), vec!["a".to_string()]),
            ("d".to_string(), vec!["b".to_string(), "c".to_string()]),
        ]);
        let g = Graph::new(&ids(&["a", "b", "c", "d"]), &deps).unwrap();
        let mut anc = g.ancestors("d");
        anc.sort();
        assert_eq!(anc, ids(&["a", "b", "c"]));
    }

    #[test]
    fn ancestors_root_has_none() {
        let deps = HashMap::from([("b".to_string(), vec!["a".to_string()])]);
        let g = Graph::new(&ids(&["a", "b"]), &deps).unwrap();
        assert!(g.ancestors("a").is_empty());
    }

    #[test]
    fn new_rejects_unknown_parent() {
        let deps = HashMap::from([("a".to_string(), vec!["ghost".to_string()])]);
        let err = Graph::new(&ids(&["a"]), &deps).unwrap_err();
        assert!(matches!(err, DagError::UnknownParent(..)));
    }

    #[test]
    fn new_rejects_dep_key_not_in_nodes() {
        let deps = HashMap::from([("ghost".to_string(), vec!["a".to_string()])]);
        let err = Graph::new(&ids(&["a"]), &deps).unwrap_err();
        assert!(matches!(err, DagError::UnknownDep(..)));
    }

    #[test]
    fn splice_rejects_duplicate_new_id() {
        let deps = HashMap::from([("b".to_string(), vec!["a".to_string()])]);
        let mut g = Graph::new(&ids(&["a", "b"]), &deps).unwrap();
        let err = g.splice("a", "a", &["b".into()]).unwrap_err();
        assert!(matches!(err, DagError::NodeExists(..)));
    }

    #[test]
    fn splice_rejects_missing_after() {
        let mut g = Graph::new(&ids(&["a"]), &HashMap::new()).unwrap();
        let err = g.splice("x", "ghost", &[]).unwrap_err();
        assert!(matches!(err, DagError::AfterMissing(..)));
    }

    #[test]
    fn splice_rejects_non_child_in_rewire() {
        let deps = HashMap::from([("b".to_string(), vec!["a".to_string()])]);
        let mut g = Graph::new(&ids(&["a", "b", "c"]), &deps).unwrap();
        let err = g.splice("x", "a", &["c".into()]).unwrap_err();
        assert!(matches!(err, DagError::NotAChild(..)));
    }

    #[test]
    fn three_node_cycle_detected() {
        let deps = HashMap::from([
            ("a".to_string(), vec!["c".to_string()]),
            ("b".to_string(), vec!["a".to_string()]),
            ("c".to_string(), vec!["b".to_string()]),
        ]);
        let err = Graph::new(&ids(&["a", "b", "c"]), &deps).unwrap_err();
        assert!(matches!(err, DagError::Cycle(..)));
    }

    #[test]
    fn empty_graph() {
        let g = Graph::new(&[], &HashMap::new()).unwrap();
        assert!(g.ready(&HashSet::new()).is_empty());
    }
}
