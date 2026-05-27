// Package lite runs LLM-driven scenarios against the in-process lite-proxy
// (LLMEndpointHandler) so we can measure how the control-notice prompt
// changes affect real agent behavior.
//
// Each scenario lives in library/<name>/ as a YAML file plus a workspace/
// subdirectory of fixture files. At test start the workspace is copied to
// t.TempDir() and the agent's bash/read/write/edit tools are sandboxed to
// that path. The scripted user delivers messages one at a time; if the
// agent fails a step's filesystem expectations, the scenario fails and no
// further user messages are sent.
//
// The harness measures, per scenario:
//
//   - task approvals granted (count of inline-approved task_create)
//   - lite-proxy tool-use blocks (postprocess refusals — currently advisory
//     because task-scope enforcement is not wired in this iteration)
//   - whether each step's filesystem expectations passed
//
// Tests skip when CLAWVISOR_LLM_API_KEY (or the legacy
// CLAWVISOR_E2E_ANTHROPIC_KEY) is unset.
package lite
