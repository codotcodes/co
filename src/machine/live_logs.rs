use super::MachineIdentity;
use crate::client;
use serde::Serialize;
use sha2::{Digest, Sha256};
use std::io::Read;
use std::sync::{
    Arc, Mutex,
    atomic::{AtomicBool, Ordering},
};
use std::thread;
use std::time::Duration;

const STEP_BYTES: usize = 16 * 1024;
const JOB_BYTES: usize = 128 * 1024;
const TRUNCATED: &str = "\n[Output truncated]\n";

#[derive(Clone, Serialize, PartialEq)]
#[serde(rename_all = "camelCase")]
pub(super) struct LiveStep {
    pub name: String,
    pub exit_code: Option<i32>,
    pub output: String,
    #[serde(skip)]
    truncated: bool,
}

pub(super) type Logs = Arc<Mutex<Vec<LiveStep>>>;

fn hash(steps: &[LiveStep]) -> String {
    let tuples: Vec<_> = steps
        .iter()
        .map(|s| (&s.name, s.exit_code, &s.output))
        .collect();
    digest(tuples)
}

pub(super) fn digest(tuples: impl Serialize) -> String {
    format!("{:x}", Sha256::digest(serde_json::to_vec(&tuples).unwrap()))
}

fn update(previous: &[LiveStep], next: &[LiveStep], repair: Option<&str>) -> serde_json::Value {
    let steps: Vec<_> = next
        .iter()
        .enumerate()
        .filter_map(|(index, step)| {
            let old = if repair.is_some() {
                None
            } else {
                previous.get(index)
            };
            if old == Some(step) {
                return None;
            }
            let offset = old.map_or(0, |s| s.output.len());
            Some(
                serde_json::json!({ "index": index, "offset": offset, "name": step.name,
            "exitCode": step.exit_code, "output": &step.output[offset..] }),
            )
        })
        .collect();
    serde_json::json!({ "baseHash": repair.map(str::to_owned).unwrap_or_else(|| hash(previous)),
        "hash": hash(next), "reset": repair.is_some(), "steps": steps })
}

pub(super) fn begin(logs: &Logs, name: &str) {
    logs.lock().unwrap().push(LiveStep {
        name: name.into(),
        exit_code: None,
        output: String::new(),
        truncated: false,
    });
}

pub(super) fn append(logs: &Logs, index: usize, text: &str) {
    let mut steps = logs.lock().unwrap();
    let used: usize = steps.iter().map(|step| step.output.len()).sum();
    // Reserve a truncation notice for each possible remaining step.
    let remaining = JOB_BYTES.saturating_sub(used + (20 - steps.len()) * TRUNCATED.len());
    let step = &mut steps[index];
    if step.truncated {
        return;
    }
    let budget = (STEP_BYTES - step.output.len()).min(remaining);
    let text: String = text
        .chars()
        .filter(|c| !c.is_control() || matches!(c, '\n' | '\t'))
        .collect();
    let available = budget.saturating_sub(TRUNCATED.len());
    let mut end = text.len().min(available);
    while !text.is_char_boundary(end) {
        end -= 1;
    }
    step.output.push_str(&text[..end]);
    if end < text.len() {
        step.output.push_str(TRUNCATED);
        step.truncated = true;
    }
}

#[cfg(test)]
pub(super) fn drain<R: Read + Send + 'static>(
    stream: R,
    logs: Logs,
    index: usize,
) -> thread::JoinHandle<()> {
    drain_masked(stream, logs, index, vec![])
}

struct SecretMasker {
    patterns: Vec<String>,
    pending: String,
}

impl SecretMasker {
    fn new(secrets: Vec<String>) -> Self {
        let mut patterns = Vec::new();
        for secret in secrets {
            for part in std::iter::once(secret.as_str()).chain(secret.lines()) {
                let normalized: String = part
                    .chars()
                    .filter(|c| !c.is_control() || matches!(c, '\n' | '\t'))
                    .collect();
                if !normalized.is_empty() && !patterns.contains(&normalized) {
                    patterns.push(normalized);
                }
            }
        }
        Self {
            patterns,
            pending: String::new(),
        }
    }

    fn feed(&mut self, text: &str, finish: bool) -> String {
        self.pending.extend(
            text.chars()
                .filter(|c| !c.is_control() || matches!(c, '\n' | '\t')),
        );
        // Retain only a suffix that could become a secret, not an arbitrary
        // fixed-size tail: ordinary short log lines still stream immediately.
        let retain = if finish {
            0
        } else {
            self.patterns
                .iter()
                .map(|pattern| {
                    let limit = self.pending.len().min(pattern.len().saturating_sub(1));
                    (1..=limit)
                        .rev()
                        .find(|length| {
                            let start = self.pending.len() - length;
                            self.pending.is_char_boundary(start)
                                && pattern.starts_with(&self.pending[start..])
                        })
                        .unwrap_or(0)
                })
                .max()
                .unwrap_or(0)
        };
        let mut end = self.pending.len() - retain;
        while !self.pending.is_char_boundary(end) {
            end -= 1;
        }
        let mut cursor = 0;
        let mut output = String::new();
        while cursor < end {
            let next = self
                .patterns
                .iter()
                .filter_map(|pattern| {
                    self.pending[cursor..]
                        .find(pattern)
                        .map(|offset| (cursor + offset, pattern.len()))
                })
                .min_by_key(|(start, length)| (*start, std::cmp::Reverse(*length)));
            if let Some((start, length)) = next.filter(|(start, _)| *start < end) {
                output.push_str(&self.pending[cursor..start]);
                output.push_str("***");
                cursor = start + length;
            } else {
                output.push_str(&self.pending[cursor..end]);
                cursor = end;
            }
        }
        self.pending.drain(..cursor);
        output
    }
}

pub(super) fn drain_masked<R: Read + Send + 'static>(
    mut stream: R,
    logs: Logs,
    index: usize,
    secrets: Vec<String>,
) -> thread::JoinHandle<()> {
    thread::spawn(move || {
        let mut masker = SecretMasker::new(secrets);
        let mut pending = Vec::new();
        let mut buffer = [0u8; 8192];
        while let Ok(count) = stream.read(&mut buffer) {
            if count == 0 {
                break;
            }
            pending.extend_from_slice(&buffer[..count]);
            // Keep incomplete UTF-8 across reads; never revise an already uploaded prefix.
            let mut consumed = 0;
            while consumed < pending.len() {
                match std::str::from_utf8(&pending[consumed..]) {
                    Ok(text) => {
                        append(&logs, index, &masker.feed(text, false));
                        consumed = pending.len();
                    }
                    Err(error) => {
                        let end = consumed + error.valid_up_to();
                        append(
                            &logs,
                            index,
                            &masker
                                .feed(std::str::from_utf8(&pending[consumed..end]).unwrap(), false),
                        );
                        consumed = end;
                        if let Some(length) = error.error_len() {
                            append(&logs, index, &masker.feed("�", false));
                            consumed += length;
                        } else {
                            break;
                        }
                    }
                }
            }
            pending.drain(..consumed);
        }
        if !pending.is_empty() {
            append(
                &logs,
                index,
                &masker.feed(&String::from_utf8_lossy(&pending), false),
            );
        }
        append(&logs, index, &masker.feed("", true));
    })
}

pub(super) fn upload(
    machine: MachineIdentity,
    id: String,
    token: String,
    logs: Logs,
    stop: Arc<AtomicBool>,
) -> thread::JoinHandle<()> {
    thread::spawn(move || {
        let mut previous = Vec::new();
        let mut warned = false;
        let mut repair: Option<String> = None;
        while !stop.load(Ordering::Relaxed) {
            let snapshot = logs.lock().unwrap().clone();
            if !snapshot.is_empty() && (snapshot != previous || repair.is_some()) {
                let update = update(&previous, &snapshot, repair.as_deref());
                let response = client().and_then(|client| {
                    client
                        .post(format!(
                            "{}/machine-enrollment/jobs/{id}/log-deltas",
                            machine.api_url
                        ))
                        .bearer_auth(&machine.credential)
                        .timeout(Duration::from_secs(5))
                        .json(&serde_json::json!({ "assignmentToken": token, "update": update }))
                        .send()
                        .map_err(|_| "live log upload failed".to_owned())
                });
                match response {
                    Ok(response) if response.status().as_u16() == 200 => {
                        let ack = response.json::<serde_json::Value>().unwrap_or_default();
                        if ack["type"] == "ack" && ack["hash"] == update["hash"] {
                            previous = snapshot;
                            repair = None;
                            warned = false;
                        }
                    }
                    Ok(response) if response.status().as_u16() == 409 => {
                        let result = response.json::<serde_json::Value>().unwrap_or_default();
                        if result["type"] == "resync" {
                            if let Some(value) = result["hash"].as_str().filter(|v| {
                                v.len() == 64 && v.bytes().all(|b| b.is_ascii_hexdigit())
                            }) {
                                repair = Some(value.to_owned());
                            }
                        } else {
                            break;
                        }
                    }
                    Ok(response) if matches!(response.status().as_u16(), 401 | 404) => break,
                    _ => {
                        if !warned {
                            eprintln!("Live log upload interrupted; retrying while the job runs.");
                            warned = true;
                        }
                    }
                }
            }
            thread::park_timeout(Duration::from_millis(500));
        }
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn secrets_are_masked_across_every_chunk_boundary_and_control_filtering() {
        for split in 0..=20 {
            let input = "before secret after!";
            let split = split.min(input.len());
            let mut masker = SecretMasker::new(vec!["secret".into()]);
            let mut result = masker.feed(&input[..split], false);
            assert!(!result.contains("secret"));
            result.push_str(&masker.feed(&input[split..], false));
            result.push_str(&masker.feed("", true));
            assert_eq!(result, "before *** after!");
        }
        let mut masker = SecretMasker::new(vec!["secret".into(), "first\nsecond".into()]);
        let mut result = masker.feed("sec\0ret first\nsecond first second", false);
        result.push_str(&masker.feed("", true));
        assert_eq!(result, "*** *** *** ***");
    }

    #[test]
    fn masking_handles_unicode_and_longest_overlapping_secret() {
        let mut masker = SecretMasker::new(vec!["🦀token".into(), "🦀".into()]);
        let mut result = masker.feed("🦀tok", false);
        result.push_str(&masker.feed("en!", true));
        assert_eq!(result, "***!");
    }
    #[test]
    fn hashes_canonical_tuples_and_sends_only_utf8_suffix() {
        assert_eq!(
            hash(&[]),
            "4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"
        );
        let logs = Logs::default();
        begin(&logs, "Test");
        append(&logs, 0, "🦀\n");
        let first = logs.lock().unwrap().clone();
        assert_eq!(
            hash(&first),
            "03032299553b8e5ce964b2b4ecbe8c3cca49ecab3919e391452793a7832c7337"
        );
        append(&logs, 0, "next\n");
        let next = logs.lock().unwrap().clone();
        let delta = update(&first, &next, None);
        assert_eq!(delta["steps"][0]["offset"], 5);
        assert_eq!(delta["steps"][0]["output"], "next\n");
        let reset = update(&first, &next, Some(&hash(&[])));
        assert_eq!(reset["steps"][0]["offset"], 0);
        assert_eq!(reset["steps"][0]["output"], "🦀\nnext\n");
        assert_eq!(reset["hash"], delta["hash"]);
    }
    #[test]
    fn output_is_bounded_and_marks_truncation() {
        let logs = Logs::default();
        for index in 0..20 {
            begin(&logs, "Step");
            append(&logs, index, &"é".repeat(STEP_BYTES));
        }
        let steps = logs.lock().unwrap();
        assert!(
            steps
                .iter()
                .all(|step| step.output.len() <= STEP_BYTES && step.output.ends_with(TRUNCATED))
        );
        assert!(steps.iter().map(|step| step.output.len()).sum::<usize>() <= JOB_BYTES);
    }

    #[test]
    fn split_utf8_preserves_append_only_output() {
        struct Bytes(std::vec::IntoIter<u8>);
        impl Read for Bytes {
            fn read(&mut self, out: &mut [u8]) -> std::io::Result<usize> {
                if let Some(byte) = self.0.next() {
                    out[0] = byte;
                    Ok(1)
                } else {
                    Ok(0)
                }
            }
        }
        let logs = Logs::default();
        begin(&logs, "UTF8");
        drain(
            Bytes("hello 🦀\n".as_bytes().to_vec().into_iter()),
            logs.clone(),
            0,
        )
        .join()
        .unwrap();
        assert_eq!(logs.lock().unwrap()[0].output, "hello 🦀\n");
    }
}
