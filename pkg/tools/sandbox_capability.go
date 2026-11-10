package tools

// ApplyRuntimePolicy probes host capabilities and disables tools that
// cannot actually run, so the model is told once instead of retrying
// until the step budget is exhausted. It does not weaken isolation or
// enable unsandboxed fallbacks.
func ApplyRuntimePolicy(sb *Sandbox) {
	if sb == nil {
		return
	}
	if ok, reason := ProbeShellIsolation(sb.Root); !ok {
		sb.DisableTool(ToolRunBash, reason)
	}
}
